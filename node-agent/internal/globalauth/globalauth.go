// Package globalauth implements bounded, Node-scoped offline verification for
// previously authorized global members. It stores pairwise member IDs only:
// never Core user IDs, usernames, plaintext passwords, or Core password hashes.
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
	MaxAuthorizationTTL   = 24 * time.Hour
	SessionTTL            = 12 * time.Hour
	MaxCachedMembers      = 1000
)

var (
	ErrNotFound        = errors.New("cached global authorization not found")
	ErrInactive        = errors.New("cached global authorization inactive")
	ErrDenied          = errors.New("global authorization denied by Core")
	ErrUnavailable     = errors.New("Core unavailable")
	ErrInvalidResponse = errors.New("invalid Core authorization response")
)

type Authorization struct {
	MemberID     string
	NodeID       string
	PasswordHash string
	Role         string
	Permissions  []string
	ValidatedAt  time.Time
	ExpiresAt    time.Time
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

// Repository atomically binds cached material and sessions to this Node.
// Applying a revoked/expired refresh must delete the authorization and cascade
// session revocation. Sessions may never outlive the authorization.
type Repository interface {
	CacheAuthorization(ctx context.Context, authorization Authorization) error
	CachedAuthorization(ctx context.Context, memberID string) (Authorization, error)
	RevokeAuthorization(ctx context.Context, memberID string) error
	CachedMemberIDs(ctx context.Context, limit int) ([]string, error)
	ApplyRefresh(ctx context.Context, nodeID string, states []RefreshState) error
	CreateGlobalSession(ctx context.Context, memberID string, digest [32]byte, ttl time.Duration, offline bool) (time.Time, error)
	GetGlobalSession(ctx context.Context, digest [32]byte) (Session, error)
	DeleteGlobalSession(ctx context.Context, digest [32]byte) error
}

func ValidMemberID(id string) bool {
	if len(id) != 71 || len(id) < 7 || id[:7] != "member_" {
		return false
	}
	for _, c := range id[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func ValidNodeID(id string) bool {
	if len(id) != 69 || len(id) < 5 || id[:5] != "node_" {
		return false
	}
	for _, c := range id[5:] {
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
func ValidAuthorization(a Authorization, now time.Time) bool {
	return ValidMemberID(a.MemberID) && ValidNodeID(a.NodeID) && ValidRole(a.Role) && ValidPermissions(a.Permissions) &&
		!a.ValidatedAt.IsZero() && !a.ExpiresAt.IsZero() && !a.ValidatedAt.After(now.Add(5*time.Minute)) &&
		a.ExpiresAt.After(now) && a.ExpiresAt.After(a.ValidatedAt) && a.ExpiresAt.Sub(a.ValidatedAt) <= MaxAuthorizationTTL && a.PasswordHash != ""
}

// HashPassword produces a fresh random-salt verifier at this Node. Core's
// stored hash is never copied, and different Nodes cannot compare verifiers.
func HashPassword(ctx context.Context, password string) (string, error) {
	return localusers.HashPassword(ctx, password)
}
func VerifyPassword(ctx context.Context, hash, password string) (bool, error) {
	return localusers.VerifyPassword(ctx, hash, password)
}
func VerifyDummy(ctx context.Context, password string) error {
	return localusers.VerifyDummy(ctx, password)
}
func ValidatePassword(password string) error      { return localusers.ValidatePassword(password) }
func NewSessionToken() (string, [32]byte, error)  { return localusers.NewSessionToken() }
func SessionDigest(token string) ([32]byte, bool) { return localusers.SessionDigest(token) }
