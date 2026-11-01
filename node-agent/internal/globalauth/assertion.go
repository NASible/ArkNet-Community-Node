package globalauth

// Node-side verifier for Core-signed login assertions (Prompt 46B,
// docs/global-login-assertions.md). The byte layout is defined by Core in
// core/internal/assertions; this file must stay byte-for-byte compatible.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const (
	AssertionDomain    = "ARKNET-NODE-LOGIN-V1\x00"
	AssertionLifetime  = 120 * time.Second
	AssertionClockSkew = 60 * time.Second
	// MaxOfflineWindow is decision D3: offline login for at most 30 days
	// since the last online verification.
	MaxOfflineWindow = 30 * 24 * time.Hour

	assertionVersion     = 1
	purposeNodeLogin     = 1
	permissionMediaByte  = 0x01
	assertionPayloadSize = 180
	minAssertionTime     = 1577836800 // 2020-01-01
	maxAssertionTime     = 7258118400 // 2200-01-01
)

var (
	ErrInvalidAssertion    = errors.New("invalid login assertion")
	ErrUnknownAssertionKey = errors.New("login assertion signed by an unknown key")
)

// KeyID is the first 8 bytes of SHA-256 of an Ed25519 public key.
type KeyID [8]byte

func KeyIDFor(public ed25519.PublicKey) KeyID {
	sum := sha256.Sum256(public)
	var id KeyID
	copy(id[:], sum[:8])
	return id
}

func (k KeyID) String() string { return hex.EncodeToString(k[:]) }

// VerificationKey is one key from Core's published assertion key set.
type VerificationKey struct {
	ID        KeyID
	PublicKey ed25519.PublicKey
}

// Expectation is what the Node knows independently of the assertion: its own
// ID and the pending login's nonce and challenge.
type Expectation struct {
	NodeID    string
	Nonce     [32]byte
	Challenge [32]byte
}

type Claims struct {
	NodeID       string
	MemberID     string
	Role         string
	Nonce        [32]byte
	Challenge    [32]byte
	AssertionID  [16]byte
	IssuedAt     time.Time
	ExpiresAt    time.Time
	OfflineUntil time.Time
}

// VerifyAssertion accepts a fresh assertion for this Node's pending login.
// Every check happens before the caller changes any state.
func VerifyAssertion(token string, keys []VerificationKey, expect Expectation, now time.Time) (Claims, error) {
	claims, err := parseSigned(token, keys)
	if err != nil {
		return Claims{}, err
	}
	switch {
	case claims.NodeID != expect.NodeID,
		subtle.ConstantTimeCompare(claims.Nonce[:], expect.Nonce[:]) != 1,
		subtle.ConstantTimeCompare(claims.Challenge[:], expect.Challenge[:]) != 1,
		claims.IssuedAt.After(now.Add(AssertionClockSkew)), !now.Before(claims.ExpiresAt.Add(AssertionClockSkew)):
		return Claims{}, ErrInvalidAssertion
	}
	return claims, nil
}

// VerifyStoredGrant re-checks a stored assertion before offline use: a
// published key, a valid signature and this Node as the audience. Login
// expiry no longer applies; the caller enforces offline_until.
func VerifyStoredGrant(token string, keys []VerificationKey, nodeID string) (Claims, error) {
	claims, err := parseSigned(token, keys)
	if err != nil {
		return Claims{}, err
	}
	if claims.NodeID != nodeID {
		return Claims{}, ErrInvalidAssertion
	}
	return claims, nil
}

func parseSigned(token string, keys []VerificationKey) (Claims, error) {
	payloadPart, signaturePart, found := strings.Cut(token, ".")
	if !found || strings.Contains(signaturePart, ".") {
		return Claims{}, ErrInvalidAssertion
	}
	payload, ok := decodeCanonical(payloadPart, assertionPayloadSize)
	signature, sigOK := decodeCanonical(signaturePart, ed25519.SignatureSize)
	if !ok || !sigOK || payload[0] != assertionVersion {
		return Claims{}, ErrInvalidAssertion
	}
	var id KeyID
	copy(id[:], payload[2:10])
	var public ed25519.PublicKey
	for _, key := range keys {
		if key.ID == id {
			public = key.PublicKey
			break
		}
	}
	if public == nil {
		return Claims{}, ErrUnknownAssertionKey
	}
	if len(public) != ed25519.PublicKeySize || !ed25519.Verify(public, append([]byte(AssertionDomain), payload...), signature) {
		return Claims{}, ErrInvalidAssertion
	}
	for _, offset := range []int{154, 162, 172} {
		if seconds := binary.BigEndian.Uint64(payload[offset:]); seconds < minAssertionTime || seconds > maxAssertionTime {
			return Claims{}, ErrInvalidAssertion
		}
	}
	claims := Claims{
		NodeID:       "node_" + hex.EncodeToString(payload[10:42]),
		MemberID:     "member_" + hex.EncodeToString(payload[42:74]),
		IssuedAt:     unixAt(payload[154:]),
		ExpiresAt:    unixAt(payload[162:]),
		OfflineUntil: unixAt(payload[172:]),
	}
	copy(claims.Nonce[:], payload[74:106])
	copy(claims.Challenge[:], payload[106:138])
	copy(claims.AssertionID[:], payload[138:154])
	switch payload[170] {
	case 1:
		claims.Role = "MEMBER"
	case 2:
		claims.Role = "ADMIN"
	}
	switch {
	case payload[1] != purposeNodeLogin, claims.Role == "", payload[171] != permissionMediaByte,
		claims.ExpiresAt.Sub(claims.IssuedAt) != AssertionLifetime,
		!claims.OfflineUntil.After(claims.IssuedAt), claims.OfflineUntil.Sub(claims.IssuedAt) > MaxOfflineWindow:
		return Claims{}, ErrInvalidAssertion
	}
	return claims, nil
}

func decodeCanonical(part string, size int) ([]byte, bool) {
	data, err := base64.RawURLEncoding.Strict().DecodeString(part)
	if err != nil || len(data) != size || base64.RawURLEncoding.EncodeToString(data) != part {
		return nil, false
	}
	return data, true
}

func unixAt(b []byte) time.Time { return time.Unix(int64(binary.BigEndian.Uint64(b)), 0).UTC() }
