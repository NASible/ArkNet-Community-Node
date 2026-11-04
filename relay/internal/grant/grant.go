// Package grant verifies the Core-signed, swarm-scoped grants the private
// tracker already uses (docs/swarm-authorization-tokens.md). The relay uses
// the same grants: a seed grant registers a listener, a download grant
// connects to one.
package grant

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"
)

const (
	Version        byte = 2
	PayloadSize         = 1 + 8 + 16 + 32 + 1
	ActionDownload byte = 0x01
	ActionSeed     byte = 0x02
	// MaxTokenBytes bounds work before decoding (a valid token is 165 bytes).
	MaxTokenBytes = 256
)

var (
	ErrInvalid = errors.New("invalid grant")
	ErrExpired = errors.New("expired grant")
	ErrAction  = errors.New("grant action not authorized")
)

type Grant struct {
	Expires time.Time
	Subject [16]byte
	Hash    [32]byte
	Actions byte
}

// Verify checks encoding, signature, version and action mask, then that the
// grant carries the required action and has not expired at now.
func Verify(token string, publicKey ed25519.PublicKey, required byte, now time.Time) (Grant, error) {
	if len(token) > MaxTokenBytes || len(publicKey) != ed25519.PublicKeySize {
		return Grant{}, ErrInvalid
	}
	encodedPayload, encodedSignature, ok := strings.Cut(token, ".")
	if !ok || strings.Contains(encodedSignature, ".") {
		return Grant{}, ErrInvalid
	}
	encoding := base64.RawURLEncoding.Strict()
	payload, err := encoding.DecodeString(encodedPayload)
	if err != nil || len(payload) != PayloadSize {
		return Grant{}, ErrInvalid
	}
	signature, err := encoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Grant{}, ErrInvalid
	}
	if !ed25519.Verify(publicKey, payload, signature) || payload[0] != Version {
		return Grant{}, ErrInvalid
	}
	actions := payload[57]
	if actions == 0 || actions&^(ActionDownload|ActionSeed) != 0 {
		return Grant{}, ErrInvalid
	}
	var result Grant
	result.Expires = time.Unix(int64(binary.BigEndian.Uint64(payload[1:9])), 0)
	copy(result.Subject[:], payload[9:25])
	copy(result.Hash[:], payload[25:57])
	result.Actions = actions
	if actions&required == 0 {
		return Grant{}, ErrAction
	}
	if !now.Before(result.Expires) {
		return Grant{}, ErrExpired
	}
	return result, nil
}
