// Package localusers defines Node-local user accounts.
//
// Local users exist only in this Node's PostgreSQL. They are never sent to
// Core, are valid only on this Node, and keep working while Core is offline.
// They share no namespace with ArkNet global users: a matching username on
// another Node, or in Core, is a different account and is never merged. IDs
// are random per account, so equal usernames never produce equal IDs.
package localusers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusDisabled Status = "DISABLED"
)

// Kind labels every local account in API responses so clients can always
// distinguish it from an ArkNet global member.
const Kind = "LOCAL"

// SessionTTL is the absolute local session lifetime; use does not extend it.
const SessionTTL = 12 * time.Hour

const (
	MinPasswordBytes = 12
	MaxPasswordBytes = 256
	MaxListedUsers   = 1000
)

var (
	ErrNotFound        = errors.New("local user not found")
	ErrUsernameTaken   = errors.New("username is already used by a local user on this node")
	ErrInactive        = errors.New("local user is not active")
	ErrInvalidUserID   = errors.New("invalid local user ID")
	ErrInvalidUsername = errors.New("invalid username")
	ErrInvalidPassword = errors.New("invalid password")
	ErrInvalidStatus   = errors.New("invalid local user status")
	ErrHashUnavailable = errors.New("password hashing unavailable")
)

type User struct {
	ID        string
	Username  string
	Status    Status
	CreatedAt time.Time
}

type Credentials struct {
	UserID       string
	PasswordHash string
	Status       Status
}

type Session struct {
	UserID    string
	ExpiresAt time.Time
}

// Repository is local-only storage; implementations must never contact Core.
//
// CreateSession must atomically require the user to be ACTIVE. Disabling a
// user must revoke all of that user's sessions in the same transaction.
// GetSession returns ErrNotFound for unknown, expired, or inactive sessions.
type Repository interface {
	CreateLocalUser(ctx context.Context, username, passwordHash string) (User, error)
	ListLocalUsers(ctx context.Context, limit int) ([]User, error)
	LocalCredentials(ctx context.Context, username string) (Credentials, error)
	SetLocalUserStatus(ctx context.Context, userID string, status Status) (User, error)
	CreateLocalSession(ctx context.Context, userID string, digest [32]byte, ttl time.Duration) (time.Time, error)
	GetLocalSession(ctx context.Context, digest [32]byte) (Session, error)
	DeleteLocalSession(ctx context.Context, digest [32]byte) error
}

func NewUserID() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "local_" + hex.EncodeToString(random[:]), nil
}

func ValidUserID(id string) bool {
	if len(id) != 70 || !strings.HasPrefix(id, "local_") {
		return false
	}
	for _, c := range id[6:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func ValidStatus(status Status) bool { return status == StatusActive || status == StatusDisabled }

// NormalizeUsername lowercases ASCII and enforces 3..32 characters of
// [a-z0-9._-], starting with a letter or digit. Uniqueness is per Node only.
func NormalizeUsername(value string) (string, error) {
	username := strings.ToLower(value)
	if len(username) < 3 || len(username) > 32 {
		return "", ErrInvalidUsername
	}
	for i, c := range []byte(username) {
		alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || c != '.' && c != '_' && c != '-') {
			return "", ErrInvalidUsername
		}
	}
	return username, nil
}

func ValidatePassword(password string) error {
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes || !utf8.ValidString(password) {
		return ErrInvalidPassword
	}
	return nil
}

// argon2id parameters follow the OWASP minimum recommendation; they match
// Core's but the code is independent because the Agent is a separate module.
const (
	argonTime           uint32 = 2
	argonMemory         uint32 = 19 * 1024 // KiB
	argonThreads        uint8  = 1
	argonSaltLen               = 16
	argonKeyLen         uint32 = 32
	maxConcurrentHashes        = 2 // ~38 MiB bound on a small community Node
)

var hashSlots = make(chan struct{}, maxConcurrentHashes)

func acquire(ctx context.Context) error {
	select {
	case hashSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ErrHashUnavailable
	}
}

func release() { <-hashSlots }

func HashPassword(ctx context.Context, password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	if err := acquire(ctx); err != nil {
		return "", err
	}
	defer release()
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	encoding := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime,
		argonThreads, encoding.EncodeToString(salt), encoding.EncodeToString(key)), nil
}

// VerifyPassword never matches a malformed or out-of-bounds stored hash.
func VerifyPassword(ctx context.Context, encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false, nil
	}
	var memory, iterations uint32
	var threads uint8
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil || n != 3 ||
		fmt.Sprintf("m=%d,t=%d,p=%d", memory, iterations, threads) != parts[3] ||
		memory < 8*1024 || memory > 256*1024 || iterations < 1 || iterations > 10 || threads < 1 || threads > 16 {
		return false, nil
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return false, nil
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, nil
	}
	if err := acquire(ctx); err != nil {
		return false, err
	}
	defer release()
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// VerifyDummy equalizes timing for unknown or malformed usernames.
func VerifyDummy(ctx context.Context, password string) error {
	dummyOnce.Do(func() {
		dummyHash, _ = HashPassword(context.Background(), "arknet-local-dummy-never-matches")
	})
	_, err := VerifyPassword(ctx, dummyHash, password)
	return err
}

func NewSessionToken() (string, [32]byte, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", [32]byte{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(random[:])
	return token, sha256.Sum256([]byte(token)), nil
}

func SessionDigest(token string) ([32]byte, bool) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(token)), true
}
