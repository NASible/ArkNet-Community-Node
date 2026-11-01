// Package memberships defines privacy-preserving Node membership invitations.
//
// A Node operator (acting through its signed Node identity) creates an
// invitation that names no user. Core returns an unguessable secret once and
// stores only its SHA-256 digest. A global user who receives the secret out of
// band authenticates to Core and accepts it, which creates the membership.
// Operators therefore never search, enumerate, or address global users.
package memberships

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

type Role string

const (
	RoleMember Role = "MEMBER"
	RoleAdmin  Role = "ADMIN"
)

const (
	MinInvitationLifetime = time.Minute
	MaxInvitationLifetime = 30 * 24 * time.Hour
	MaxInvitationUses     = 100
)

var (
	ErrInvalidRole       = errors.New("invalid membership role")
	ErrInvalidExpiration = errors.New("invalid invitation expiration")
	ErrInvalidMaxUses    = errors.New("invalid invitation maximum uses")
	// ErrUnavailable deliberately covers unknown, expired, exhausted, and
	// untrusted-node invitations so acceptance reveals nothing more.
	ErrUnavailable    = errors.New("invitation is invalid, expired, or exhausted")
	ErrAlreadyMember  = errors.New("user is already a member of this node")
	ErrReplayedCreate = errors.New("invitation request was already processed")
	ErrNodeNotFound   = errors.New("node not found")
	// ErrAuthorizationDenied deliberately covers unknown membership, inactive
	// user, and wrong Node scope so a Node cannot enumerate global identities.
	ErrAuthorizationDenied = errors.New("global authorization denied")
)

type Invitation struct {
	ID        string
	NodeID    string
	Role      Role
	MaxUses   int
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Membership is the user-facing acceptance result.
type Membership struct {
	NodeID    string
	MemberID  string
	Role      Role
	CreatedAt time.Time
}

// Member is an internal/test representation of one membership. MemberID is a
// pairwise pseudonym: random per (user, Node) pair, generated independently
// for each Node, with no derivation from the global user ID. Core alone holds
// the mapping; no Node-facing bulk membership listing is exposed.
type Member struct {
	MemberID string
	Role     Role
	JoinedAt time.Time
}

// AuthorizationState is the Node-safe current state used to refresh a cache.
type AuthorizationState struct {
	MemberID   string
	Authorized bool
	Role       Role
}

var (
	// ErrHomeNodeUnavailable covers "not a member", unknown Node, untrusted
	// Node, and inactive user, so a home-node request cannot probe Nodes.
	ErrHomeNodeUnavailable = errors.New("home node is unavailable")
	// ErrNoHomeNode means no currently usable home node is set.
	ErrNoHomeNode = errors.New("no home node is set")
)

// HomeNode is Core-internal routing state: the user's one preferred Node and
// the pairwise member ID valid only on that Node. It is never sent to Nodes.
type HomeNode struct {
	NodeID   string
	MemberID string
	SetAt    time.Time
}

// HomeNodeRepository stores at most one home node per global user. Only
// SetHomeNode changes it; accepting invitations never does. HomeNode returns
// ErrNoHomeNode unless the user is ACTIVE and the Node is APPROVED/TRUSTED.
type HomeNodeRepository interface {
	SetHomeNode(ctx context.Context, userID, nodeID string) (HomeNode, error)
	HomeNode(ctx context.Context, userID string) (HomeNode, error)
}

// MaxAuthorizationRefreshMembers bounds one Node's per-member refresh request.
const MaxAuthorizationRefreshMembers = 1000

// Repository persists invitations and memberships.
//
// CreateInvitation must reject a reused creationProof (ErrReplayedCreate) so a
// captured signed request cannot mint extra invitations. AcceptInvitation must,
// in one transaction, lock the invitation, require it to be unexpired with
// remaining uses and its Node approved and trusted, require the user to be
// ACTIVE, create the membership, and consume one use. A user who is already a
// member gets ErrAlreadyMember and consumes no use.
type Repository interface {
	CreateInvitation(ctx context.Context, nodeID string, role Role, expiresAt time.Time, maxUses int, secretDigest, creationProof [32]byte) (Invitation, error)
	AcceptInvitation(ctx context.Context, secretDigest [32]byte, userID string) (Membership, error)
}

// OfflineAuthorizationRepository answers a reconnected Node's refresh of the
// pairwise members it holds grants for. It never exposes authentication
// material (ARKSEC-001: Core no longer verifies Node-relayed passwords).
type OfflineAuthorizationRepository interface {
	AuthorizationStates(ctx context.Context, nodeID string, memberIDs []string) ([]AuthorizationState, error)
}

func ValidRole(role Role) bool { return role == RoleMember || role == RoleAdmin }

// ValidatePolicy bounds operator-chosen policy against the current time.
func ValidatePolicy(role Role, expiresAt time.Time, maxUses int, now time.Time) error {
	if !ValidRole(role) {
		return ErrInvalidRole
	}
	if expiresAt.Before(now.Add(MinInvitationLifetime)) || expiresAt.After(now.Add(MaxInvitationLifetime)) {
		return ErrInvalidExpiration
	}
	if maxUses < 1 || maxUses > MaxInvitationUses {
		return ErrInvalidMaxUses
	}
	return nil
}

func NewInvitationID() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "invite_" + hex.EncodeToString(random[:]), nil
}

func ValidInvitationID(id string) bool {
	if len(id) != 71 || !strings.HasPrefix(id, "invite_") {
		return false
	}
	for _, c := range id[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// NewMemberID returns a fresh pairwise membership ID from 256 random bits.
// It is deliberately not derived (for example by HMAC) from the user or Node,
// so no key compromise or Node collusion can map it back to a global user.
func NewMemberID() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "member_" + hex.EncodeToString(random[:]), nil
}

func ValidMemberID(id string) bool {
	if len(id) != 71 || !strings.HasPrefix(id, "member_") {
		return false
	}
	for _, c := range id[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// NewSecret returns a 256-bit secret and the only form Core stores. The secret
// is high-entropy, so a fast digest is sufficient; no password hash is needed.
func NewSecret() (string, [32]byte, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", [32]byte{}, err
	}
	secret := base64.RawURLEncoding.EncodeToString(random[:])
	return secret, sha256.Sum256([]byte(secret)), nil
}

// SecretDigest validates canonical secret syntax before digesting.
func SecretDigest(secret string) ([32]byte, bool) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(secret)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != secret {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(secret)), true
}
