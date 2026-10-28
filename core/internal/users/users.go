// Package users defines ArkNet-owned global user accounts and sessions.
//
// Identity is local to Core: there is no external identity provider. User IDs
// are opaque random values carrying no username or operator input. Passwords
// are stored only as argon2id hashes, and session tokens only as SHA-256
// digests, so a database disclosure reveals neither credentials nor sessions.
package users

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

// SessionTTL is the absolute session lifetime. Sessions are not extended by use.
const SessionTTL = 12 * time.Hour

const (
	MinPasswordBytes = 12
	MaxPasswordBytes = 256
)

var (
	ErrNotFound        = errors.New("user not found")
	ErrUsernameTaken   = errors.New("username is already registered")
	ErrInactive        = errors.New("user is not active")
	ErrInvalidUserID   = errors.New("invalid user ID")
	ErrInvalidUsername = errors.New("invalid username")
	ErrInvalidPassword = errors.New("invalid password")
	ErrInvalidStatus   = errors.New("invalid user status")
	ErrHashUnavailable = errors.New("password hashing unavailable")
)

type User struct {
	ID        string
	Username  string
	Status    Status
	CreatedAt time.Time
}

// Credentials is the minimum needed to authenticate a login attempt.
type Credentials struct {
	UserID       string
	PasswordHash string
	Status       Status
}

type Session struct {
	UserID    string
	ExpiresAt time.Time
}

// Repository stores users and session digests.
//
// CreateSession must atomically require the user to be ACTIVE, so a user
// disabled after password verification still cannot obtain a session.
// Setting a user DISABLED must revoke all of that user's sessions in the same
// transaction. GetSession returns ErrNotFound for unknown or expired sessions
// and for sessions of users who are not ACTIVE.
type Repository interface {
	CreateUser(ctx context.Context, username, passwordHash string) (User, error)
	GetCredentials(ctx context.Context, username string) (Credentials, error)
	SetUserStatus(ctx context.Context, userID string, status Status) (User, error)
	CreateSession(ctx context.Context, userID string, digest [32]byte, ttl time.Duration) (time.Time, error)
	GetSession(ctx context.Context, digest [32]byte) (Session, error)
	DeleteSession(ctx context.Context, digest [32]byte) error
	GetCredentialsByID(ctx context.Context, userID string) (Credentials, error)
	// ReplacePassword swaps currentHash for newHash only if currentHash is still
	// stored (ErrNotFound otherwise) and the user is ACTIVE (ErrInactive), and
	// revokes every session of that user in the same transaction.
	ReplacePassword(ctx context.Context, userID, currentHash, newHash string) error
}

// NewUserID uses 256 random bits and no user-supplied input.
func NewUserID() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "user_" + hex.EncodeToString(random[:]), nil
}

func ValidUserID(id string) bool {
	if len(id) != 69 || !strings.HasPrefix(id, "user_") {
		return false
	}
	for _, c := range id[5:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func ValidStatus(status Status) bool { return status == StatusActive || status == StatusDisabled }

// NormalizeUsername lowercases ASCII and enforces 3..32 characters of
// [a-z0-9._-], starting with a letter or digit. Usernames are login handles
// only; they are never sent to Nodes, which see pairwise pseudonyms instead.
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

// ValidatePassword applies length bounds only (no composition rules).
func ValidatePassword(password string) error {
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes || !utf8.ValidString(password) {
		return ErrInvalidPassword
	}
	return nil
}

// argon2id parameters follow the OWASP minimum recommendation.
const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 19 * 1024 // KiB
	argonThreads uint8  = 1
	argonSaltLen        = 16
	argonKeyLen  uint32 = 32
	// Each hash holds ~19 MiB; bounding concurrency bounds login memory.
	maxConcurrentHashes = 4
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

// HashPassword returns a PHC-format argon2id string with a random salt.
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

// VerifyPassword reports whether password matches encoded. A malformed or
// out-of-bounds stored hash never matches; bounds stop a corrupted row from
// forcing unbounded memory or time.
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

// VerifyDummy spends the same work as a real verification so unknown
// usernames are not distinguishable by response time.
func VerifyDummy(ctx context.Context, password string) error {
	dummyOnce.Do(func() {
		dummyHash, _ = HashPassword(context.Background(), "arknet-dummy-password-never-matches")
	})
	_, err := VerifyPassword(ctx, dummyHash, password)
	return err
}

// NewSessionToken returns a 256-bit bearer token and the digest to store.
func NewSessionToken() (string, [32]byte, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", [32]byte{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(random[:])
	return token, sha256.Sum256([]byte(token)), nil
}

// SessionDigest validates canonical token syntax before digesting.
func SessionDigest(token string) ([32]byte, bool) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(token)), true
}
