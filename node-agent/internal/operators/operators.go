// Package operators defines Node operator accounts: the people who manage a
// community Node through its management UI.
//
// Operator accounts exist only in this Node's PostgreSQL, are never sent to
// Core and work while Core is offline. They share nothing with global members
// or local users: separate tables, an "op_" ID prefix, and cookie-only
// sessions. See docs/node-operator-authentication.md.
//
// Every secret here (setup and recovery codes, enrollment and session tokens)
// is shown once and stored only as a SHA-256 digest. The TOTP secret is the
// exception: it must be readable to verify codes.
package operators

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"arknet/node-agent/internal/localusers"
)

type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusDisabled Status = "DISABLED"
)

// Purpose distinguishes a first or additional operator (SETUP) from
// re-enrolling an existing operator after shell recovery (RECOVERY).
type Purpose string

const (
	PurposeSetup    Purpose = "SETUP"
	PurposeRecovery Purpose = "RECOVERY"
)

// Policy values from the design (decisions Q5 and Q8).
const (
	SessionIdleTTL         = 30 * time.Minute
	SessionTTL             = 12 * time.Hour
	StepUpWindow           = 10 * time.Minute
	MaxSessionsPerOperator = 10
	SetupCodeTTL           = time.Hour
	EnrollmentTTL          = 15 * time.Minute
	RecoveryCodeCount      = 10
	FailureThreshold       = 5
	MaxFailureDelay        = 15 * time.Minute
	MaxListedOperators     = 100

	totpPeriod = 30
	totpDigits = 6
	// TOTPSecretBytes is the RFC 4226 recommended 160-bit secret length.
	TOTPSecretBytes  = 20
	setupCodeBytes   = 20 // 32 base32 characters
	recoveryCodeLen  = 10 // base32 characters
	totpIssuer       = "ArkNet Node"
	codeAlphabet     = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	setupCodeLength  = setupCodeBytes * 8 / 5
	setupCodeGroupSz = 4
)

var (
	ErrNotFound       = errors.New("operator not found")
	ErrInvalidCode    = errors.New("code is unknown, used or expired")
	ErrOperatorsExist = errors.New("an active operator already exists")
	ErrUsernameTaken  = errors.New("username is already used by an operator on this node")
	ErrInactive       = errors.New("operator is not active")
	ErrReplay         = errors.New("second factor was already used")
	ErrInvalidID      = errors.New("invalid operator ID")
)

type Operator struct {
	ID           string
	Username     string
	Status       Status
	CreatedAt    time.Time
	LastSignInAt *time.Time
}

// Credentials is everything a login check needs. RetryAfter is set while the
// failure delay is in force.
type Credentials struct {
	OperatorID   string
	Username     string
	PasswordHash string
	TOTPSecret   []byte
	TOTPLastStep int64
	Status       Status
	RetryAfter   *time.Time
}

type Session struct {
	OperatorID      string
	Username        string
	AuthenticatedAt time.Time
	IdleExpiresAt   time.Time
	ExpiresAt       time.Time
}

// Enrollment is a started setup or recovery waiting for TOTP confirmation.
type Enrollment struct {
	Purpose    Purpose
	Username   string
	TOTPSecret []byte
	ExpiresAt  time.Time
}

// SecondFactor is exactly one of a verified TOTP step or a recovery-code digest.
type SecondFactor struct {
	TOTPStep       int64
	RecoveryDigest *[32]byte
}

// Repository is local-only storage; implementations never contact Core.
//
// Codes and tokens are single use: consuming them is a conditional update in
// the same transaction as the effect. Issuing a recovery code revokes the
// operator's sessions immediately. Disabling revokes sessions in the same
// transaction. GetSession returns ErrNotFound for unknown, idle-expired,
// absolutely expired or inactive sessions, and otherwise extends the idle
// deadline (never past the absolute expiry).
type Repository interface {
	CountActiveOperators(ctx context.Context) (int, error)
	IssueSetupCode(ctx context.Context, digest [32]byte, additional bool) (time.Time, error)
	IssueRecoveryCode(ctx context.Context, username string, digest [32]byte) (time.Time, error)
	BeginEnrollment(ctx context.Context, codeDigest [32]byte, username, passwordHash string, totpSecret []byte, enrollmentDigest [32]byte) (Enrollment, error)
	PendingEnrollment(ctx context.Context, enrollmentDigest [32]byte) (Enrollment, error)
	ConfirmEnrollment(ctx context.Context, enrollmentDigest [32]byte, totpStep int64, recoveryDigests [][32]byte, sessionDigest [32]byte) (Session, error)
	OperatorCredentials(ctx context.Context, username string) (Credentials, error)
	RecordFailedLogin(ctx context.Context, operatorID string) error
	CreateOperatorSession(ctx context.Context, operatorID string, factor SecondFactor, sessionDigest [32]byte) (Session, error)
	GetOperatorSession(ctx context.Context, digest [32]byte) (Session, error)
	DeleteOperatorSession(ctx context.Context, digest [32]byte) error
	ListOperators(ctx context.Context) ([]Operator, error)
	DisableOperator(ctx context.Context, username string) error
}

func NewOperatorID() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "op_" + hex.EncodeToString(random[:]), nil
}

func ValidOperatorID(id string) bool {
	if len(id) != 67 || !strings.HasPrefix(id, "op_") {
		return false
	}
	for _, c := range id[3:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Usernames and passwords follow the local-user policy.
func NormalizeUsername(value string) (string, error) { return localusers.NormalizeUsername(value) }
func ValidatePassword(password string) error         { return localusers.ValidatePassword(password) }
func HashPassword(ctx context.Context, password string) (string, error) {
	return localusers.HashPassword(ctx, password)
}
func VerifyPassword(ctx context.Context, encoded, password string) (bool, error) {
	return localusers.VerifyPassword(ctx, encoded, password)
}
func VerifyDummy(ctx context.Context, password string) error {
	return localusers.VerifyDummy(ctx, password)
}

// NewToken returns a 32-byte opaque token (sessions, enrollments) and its digest.
func NewToken() (string, [32]byte, error) { return localusers.NewSessionToken() }

// TokenDigest validates the canonical token form before hashing.
func TokenDigest(token string) ([32]byte, bool) { return localusers.SessionDigest(token) }

func randomCode(n int) (string, error) {
	random := make([]byte, n)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range random {
		out[i] = codeAlphabet[b&31] // 256 is a multiple of 32: no modulo bias
	}
	return string(out), nil
}

func group(code string, size int) string {
	parts := make([]string, 0, len(code)/size+1)
	for len(code) > size {
		parts, code = append(parts, code[:size]), code[size:]
	}
	return strings.Join(append(parts, code), "-")
}

// NewSetupCode returns a display code such as ABCD-EFGH-… and its digest.
// The same format is used for SETUP and RECOVERY codes.
func NewSetupCode() (string, [32]byte, error) {
	random := make([]byte, setupCodeBytes)
	if _, err := rand.Read(random); err != nil {
		return "", [32]byte{}, err
	}
	raw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random)
	return group(raw, setupCodeGroupSz), sha256.Sum256([]byte(raw)), nil
}

// SetupCodeDigest accepts the display form, any case, with or without dashes
// and spaces.
func SetupCodeDigest(input string) ([32]byte, bool) {
	return codeDigest(input, setupCodeLength)
}

// NewRecoveryCodes returns RecoveryCodeCount display codes and their digests.
func NewRecoveryCodes() ([]string, [][32]byte, error) {
	codes, digests := make([]string, RecoveryCodeCount), make([][32]byte, RecoveryCodeCount)
	for i := range codes {
		raw, err := randomCode(recoveryCodeLen)
		if err != nil {
			return nil, nil, err
		}
		codes[i], digests[i] = group(raw, recoveryCodeLen/2), sha256.Sum256([]byte(raw))
	}
	return codes, digests, nil
}

func RecoveryCodeDigest(input string) ([32]byte, bool) { return codeDigest(input, recoveryCodeLen) }

func codeDigest(input string, length int) ([32]byte, bool) {
	raw := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(input))
	if len(raw) != length {
		return [32]byte{}, false
	}
	for _, c := range raw {
		if !strings.ContainsRune(codeAlphabet, c) {
			return [32]byte{}, false
		}
	}
	return sha256.Sum256([]byte(raw)), true
}

func NewTOTPSecret() ([]byte, error) {
	secret := make([]byte, TOTPSecretBytes)
	_, err := rand.Read(secret)
	return secret, err
}

func EncodeTOTPSecret(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

// TOTPURI is the otpauth:// provisioning URI authenticator apps import.
func TOTPURI(username string, secret []byte) string {
	label := url.PathEscape(totpIssuer + ":" + username)
	query := url.Values{"secret": {EncodeTOTPSecret(secret)}, "issuer": {totpIssuer}, "algorithm": {"SHA1"},
		"digits": {fmt.Sprint(totpDigits)}, "period": {fmt.Sprint(totpPeriod)}}
	return "otpauth://totp/" + label + "?" + query.Encode()
}

// TOTPCode is the RFC 6238 HOTP value for a time step (HMAC-SHA1, 6 digits).
func TOTPCode(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, value%1_000_000)
}

func TOTPStep(now time.Time) int64 { return now.Unix() / totpPeriod }

// VerifyTOTP accepts the current step ±1 and only steps newer than lastStep,
// so an accepted code can never be replayed. It returns the matched step.
func VerifyTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	if len(code) != totpDigits || strings.Trim(code, "0123456789") != "" || len(secret) != TOTPSecretBytes {
		return 0, false
	}
	current, matched := TOTPStep(now), int64(0)
	for step := current - 1; step <= current+1; step++ {
		if subtle.ConstantTimeCompare([]byte(TOTPCode(secret, step)), []byte(code)) == 1 && step > lastStep {
			matched = step
		}
	}
	return matched, matched != 0
}

// FailureDelay is how long sign-in is refused after n consecutive failures:
// nothing below FailureThreshold, then 1 s doubling up to MaxFailureDelay.
func FailureDelay(failures int) time.Duration {
	if failures < FailureThreshold {
		return 0
	}
	shift := failures - FailureThreshold
	if shift >= 10 {
		return MaxFailureDelay
	}
	return min(time.Second<<shift, MaxFailureDelay)
}
