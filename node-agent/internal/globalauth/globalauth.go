// Package globalauth signs global members in to this Node with Core-signed,
// Node-bound login assertions (Prompt 46B, docs/global-login-assertions.md).
// The Node never sees a global password. It stores pairwise member IDs, the
// verified assertion as the offline grant, and digests of browser-held login
// state, device credentials and session tokens; never Core user IDs,
// usernames, passwords or password-derived verifiers.
package globalauth

import (
	"context"
	"errors"
	"time"

	"arknet/node-agent/internal/localusers"
)

const (
	Kind                  = "GLOBAL"
	PermissionMediaAccess = "MEDIA_ACCESS"
	SessionTTL            = 12 * time.Hour
	MaxCachedMembers      = 1000
	// PendingLoginTTL bounds a login started on this Node (Node clock only).
	PendingLoginTTL = 5 * time.Minute
	// MaxPendingLogins bounds unauthenticated login starts.
	MaxPendingLogins = 1000
	// MaxDevicesPerMember bounds device credentials; the oldest are dropped.
	MaxDevicesPerMember = 10
)

var (
	ErrNotFound        = errors.New("global login state not found")
	ErrInactive        = errors.New("global authorization inactive")
	ErrDenied          = errors.New("global authorization denied by Core")
	ErrUnavailable     = errors.New("Core unavailable")
	ErrInvalidResponse = errors.New("invalid Core response")
	ErrReplay          = errors.New("login assertion already used")
	ErrBusy            = errors.New("too many pending global logins")
)

// Grant is a verified assertion kept as the offline grant (Option A). Every
// field except Assertion is a copy of a Core-signed claim.
type Grant struct {
	MemberID     string
	NodeID       string
	Role         string
	IssuedAt     time.Time
	OfflineUntil time.Time
	Assertion    string
}

type RefreshState struct {
	MemberID    string
	Authorized  bool
	Role        string
	Permissions []string
	ValidatedAt time.Time
	ExpiresAt   time.Time
}

type Session struct {
	MemberID    string
	Permissions []string
	Offline     bool
	ExpiresAt   time.Time
}

// CompletedLogin is everything an accepted online login writes, atomically.
type CompletedLogin struct {
	// Challenge identifies the pending login (SHA-256 of the browser's login
	// state cookie); it is consumed.
	Challenge          [32]byte
	AssertionID        [16]byte
	AssertionExpiresAt time.Time
	Grant              Grant
	// DeviceDigest is the SHA-256 of the new device credential. A previous
	// credential presented by the same browser is replaced.
	DeviceDigest   [32]byte
	ReplacedDevice *[32]byte
	SessionDigest  [32]byte
	SessionTTL     time.Duration
}

// Repository binds all login state to this Node.
//
// CompleteLogin consumes the pending login, records the assertion ID (ErrReplay
// if already present), stores the grant (a newer grant replaces an older one),
// the device credential and the session, all in one transaction; ErrNotFound
// if the pending login is gone. OfflineGrant returns the grant for a device
// credential only while it is valid and the clock has not gone backwards past
// the newest issued_at minus the skew. ApplyRefresh may only shorten or revoke.
type Repository interface {
	AssertionKeys(ctx context.Context) ([]VerificationKey, error)
	ReplaceAssertionKeys(ctx context.Context, keys []VerificationKey) error
	BeginLogin(ctx context.Context, challenge, nonce [32]byte, ttl time.Duration) error
	PendingLoginNonce(ctx context.Context, challenge [32]byte) ([32]byte, error)
	CompleteLogin(ctx context.Context, login CompletedLogin) (time.Time, error)
	OfflineGrant(ctx context.Context, deviceDigest [32]byte) (Grant, error)
	CreateOfflineSession(ctx context.Context, deviceDigest, sessionDigest [32]byte, ttl time.Duration) (time.Time, error)
	RevokeAuthorization(ctx context.Context, memberID string) error
	CachedMemberIDs(ctx context.Context, limit int) ([]string, error)
	ApplyRefresh(ctx context.Context, nodeID string, states []RefreshState) error
	GetGlobalSession(ctx context.Context, digest [32]byte) (Session, error)
	DeleteGlobalSession(ctx context.Context, digest [32]byte) error
}

func ValidMemberID(id string) bool { return validPrefixedHex(id, "member_") }
func ValidNodeID(id string) bool   { return validPrefixedHex(id, "node_") }

func validPrefixedHex(id, prefix string) bool {
	if len(id) != len(prefix)+64 || id[:len(prefix)] != prefix {
		return false
	}
	for _, c := range id[len(prefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func ValidRole(role string) bool { return role == "MEMBER" || role == "ADMIN" }
func ValidPermissions(values []string) bool {
	return len(values) == 1 && values[0] == PermissionMediaAccess
}

// ValidGrant bounds a grant by D3: offline login ends at most 30 days after the
// online verification that produced it.
func ValidGrant(g Grant, now time.Time) bool {
	return ValidMemberID(g.MemberID) && ValidNodeID(g.NodeID) && ValidRole(g.Role) && g.Assertion != "" &&
		!g.IssuedAt.IsZero() && g.OfflineUntil.After(g.IssuedAt) && g.OfflineUntil.Sub(g.IssuedAt) <= MaxOfflineWindow &&
		g.OfflineUntil.After(now)
}

func NewSessionToken() (string, [32]byte, error)  { return localusers.NewSessionToken() }
func SessionDigest(token string) ([32]byte, bool) { return localusers.SessionDigest(token) }
