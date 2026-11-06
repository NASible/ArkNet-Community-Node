// Package admins models Core administrator identities, their scoped API tokens
// and the append-only administrative audit log (ARKSEC-005, Prompt 49).
//
// Tokens are minted only from a shell on the Core host. Core stores the SHA-256
// digest of each token, never the token itself.
package admins

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Permission is one route-scoped administrative capability.
type Permission string

const (
	NodesDecide Permission = "nodes.decide"
	UsersAdmin  Permission = "users.admin"
	MediaAdmin  Permission = "media.admin"
	RelaysRead  Permission = "relays.read"
)

const (
	// TokenPrefix makes leaked tokens recognisable to secret scanners.
	TokenPrefix = "arknet_adm_"
	DefaultTTL  = 30 * 24 * time.Hour
	MaxTTL      = 90 * 24 * time.Hour
	minTTL      = time.Minute
)

// Audit outcomes. "authorized" is written before the action runs; the action
// does not run unless that row is stored.
const (
	OutcomeDenied     = "denied"
	OutcomeAuthorized = "authorized"
	OutcomeCompleted  = "completed"
	OutcomeMinted     = "token_minted"
	OutcomeRevoked    = "token_revoked"
)

var (
	ErrNotFound      = errors.New("administrator token not found")
	ErrInvalidName   = errors.New("invalid administrator name")
	ErrInvalidPolicy = errors.New("invalid permissions or lifetime")
)

var allPermissions = []Permission{MediaAdmin, NodesDecide, RelaysRead, UsersAdmin}

func AllPermissions() []Permission { return append([]Permission(nil), allPermissions...) }

func ParsePermission(value string) (Permission, bool) {
	for _, p := range allPermissions {
		if string(p) == value {
			return p, true
		}
	}
	return "", false
}

// NormalizePermissions requires a non-empty set of known, unique permissions
// and returns them sorted.
func NormalizePermissions(values []string) ([]Permission, error) {
	if len(values) == 0 {
		return nil, ErrInvalidPolicy
	}
	seen := map[Permission]bool{}
	out := make([]Permission, 0, len(values))
	for _, value := range values {
		p, ok := ParsePermission(value)
		if !ok || seen[p] {
			return nil, ErrInvalidPolicy
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func ValidTTL(ttl time.Duration) bool { return ttl >= minTTL && ttl <= MaxTTL }

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

// ValidName accepts an operator-chosen administrator label. Labels that look
// like global user IDs are refused so the two identity classes never mix.
func ValidName(name string) bool {
	return namePattern.MatchString(name) && !strings.HasPrefix(name, "user_")
}

// NewToken returns a fresh bearer token and its digest.
func NewToken() (string, [32]byte, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", [32]byte{}, err
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(random[:])
	return token, sha256.Sum256([]byte(token)), nil
}

// ParseToken validates canonical syntax before digesting.
func ParseToken(token string) ([32]byte, bool) {
	encoded, found := strings.CutPrefix(token, TokenPrefix)
	if !found {
		return [32]byte{}, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(token)), true
}

func newID(prefix string) (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func validID(prefix, id string) bool {
	rest, found := strings.CutPrefix(id, prefix)
	if !found || len(rest) != 64 {
		return false
	}
	for _, c := range rest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func NewAdminID() (string, error) { return newID("admin_") }
func NewTokenID() (string, error) { return newID("admtok_") }
func ValidAdminID(id string) bool { return validID("admin_", id) }
func ValidTokenID(id string) bool { return validID("admtok_", id) }

// Principal is an authenticated, unexpired, unrevoked administrator token.
type Principal struct {
	AdminID     string
	AdminName   string
	TokenID     string
	Permissions []Permission
	ExpiresAt   time.Time
}

func (p Principal) Has(permission Permission) bool {
	for _, granted := range p.Permissions {
		if granted == permission && permission != "" {
			return true
		}
	}
	return false
}

// Token describes a stored token. It never contains the bearer secret.
type Token struct {
	TokenID     string
	AdminID     string
	AdminName   string
	Permissions []Permission
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// AuditEntry is one append-only audit row. Action is the route pattern and
// Target the opaque resource ID; neither ever holds a secret or request body.
type AuditEntry struct {
	AdminID    string
	TokenID    string
	Permission Permission
	Action     string
	Target     string
	Outcome    string
	Status     int
}

// Repository is the administrator store. Authenticate returns ErrNotFound for
// unknown, expired and revoked tokens alike. CreateToken creates the named
// administrator on first use. AppendAudit only ever inserts.
type Repository interface {
	Authenticate(ctx context.Context, digest [32]byte) (Principal, error)
	CreateToken(ctx context.Context, adminName string, permissions []Permission, ttl time.Duration, digest [32]byte) (Token, error)
	RevokeToken(ctx context.Context, tokenID string) error
	AppendAudit(ctx context.Context, entry AuditEntry) error
}
