// Package revocation defines the Core-signed message that tells one relay to
// cut the sessions of revoked pairwise subjects (Prompt 48A; Core issues it
// in Prompt 48B).
//
// Token: base64url(payload) "." base64url(Ed25519(Domain || payload)), with a
// dedicated revocation key (never the tracker-grant or assertion key).
//
//	offset size  field
//	0      1     version (1)
//	1      8     issued_at, Unix seconds, big-endian
//	9      16    revocation ID (random; single use)
//	25     32    audience: SHA-256 of the relay ID
//	57     2     subject count, big-endian (1..MaxSubjects)
//	59     16*n  pairwise subjects (as in tracker grants)
package revocation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	Domain      = "ARKNET-RELAY-REVOCATION-V1\x00"
	Version     = 1
	headerSize  = 59
	MaxSubjects = 4096
	// MaxAge and ClockSkew bound how old, or how far ahead of this relay's
	// clock, an accepted revocation may be.
	MaxAge    = 60 * time.Second
	ClockSkew = 60 * time.Second
	// MaxTokenBytes fits MaxSubjects subjects with margin.
	MaxTokenBytes    = 96 << 10
	maxReplayEntries = 1 << 16
)

var (
	ErrInvalid  = errors.New("invalid revocation")
	ErrAudience = errors.New("revocation is for another relay")
	ErrStale    = errors.New("revocation is not fresh")
	ErrReplay   = errors.New("revocation already applied")
	ErrBusy     = errors.New("too many recent revocations")
)

type Revocation struct {
	ID       [16]byte
	IssuedAt time.Time
	Subjects [][16]byte
}

func audience(relayID string) [32]byte { return sha256.Sum256([]byte(relayID)) }

// Sign creates a revocation token. Core uses the same layout (Prompt 48B).
func Sign(key ed25519.PrivateKey, relayID string, id [16]byte, issuedAt time.Time, subjects [][16]byte) (string, error) {
	if len(subjects) == 0 || len(subjects) > MaxSubjects || len(key) != ed25519.PrivateKeySize || relayID == "" {
		return "", ErrInvalid
	}
	payload := make([]byte, headerSize, headerSize+16*len(subjects))
	payload[0] = Version
	binary.BigEndian.PutUint64(payload[1:9], uint64(issuedAt.Unix()))
	copy(payload[9:25], id[:])
	aud := audience(relayID)
	copy(payload[25:57], aud[:])
	binary.BigEndian.PutUint16(payload[57:59], uint16(len(subjects)))
	for _, subject := range subjects {
		payload = append(payload, subject[:]...)
	}
	signature := ed25519.Sign(key, append([]byte(Domain), payload...))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Verify checks the signature, layout, audience and freshness. Replays within
// the freshness window are the ReplayGuard's job.
func Verify(token string, key ed25519.PublicKey, relayID string, now time.Time) (Revocation, error) {
	if len(token) > MaxTokenBytes || len(key) != ed25519.PublicKeySize {
		return Revocation{}, ErrInvalid
	}
	encodedPayload, encodedSignature, ok := strings.Cut(token, ".")
	if !ok || strings.Contains(encodedSignature, ".") {
		return Revocation{}, ErrInvalid
	}
	encoding := base64.RawURLEncoding.Strict()
	payload, err := encoding.DecodeString(encodedPayload)
	if err != nil || len(payload) < headerSize {
		return Revocation{}, ErrInvalid
	}
	signature, err := encoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize ||
		!ed25519.Verify(key, append([]byte(Domain), payload...), signature) {
		return Revocation{}, ErrInvalid
	}
	count := int(binary.BigEndian.Uint16(payload[57:59]))
	if payload[0] != Version || count == 0 || count > MaxSubjects || len(payload) != headerSize+16*count {
		return Revocation{}, ErrInvalid
	}
	aud := audience(relayID)
	if string(payload[25:57]) != string(aud[:]) {
		return Revocation{}, ErrAudience
	}
	result := Revocation{IssuedAt: time.Unix(int64(binary.BigEndian.Uint64(payload[1:9])), 0), Subjects: make([][16]byte, count)}
	copy(result.ID[:], payload[9:25])
	if result.IssuedAt.Before(now.Add(-MaxAge)) || result.IssuedAt.After(now.Add(ClockSkew)) {
		return Revocation{}, ErrStale
	}
	for i := range result.Subjects {
		copy(result.Subjects[i][:], payload[headerSize+16*i:])
	}
	return result, nil
}

// ReplayGuard remembers applied revocation IDs for as long as Verify would
// still accept them.
type ReplayGuard struct {
	mu   sync.Mutex
	seen map[[16]byte]time.Time // ID -> forget after
}

func NewReplayGuard() *ReplayGuard { return &ReplayGuard{seen: map[[16]byte]time.Time{}} }

func (g *ReplayGuard) Accept(r Revocation, now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, forget := range g.seen {
		if now.After(forget) {
			delete(g.seen, id)
		}
	}
	if _, ok := g.seen[r.ID]; ok {
		return ErrReplay
	}
	if len(g.seen) >= maxReplayEntries {
		return ErrBusy
	}
	g.seen[r.ID] = r.IssuedAt.Add(MaxAge + ClockSkew)
	return nil
}

func (g *ReplayGuard) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen)
}
