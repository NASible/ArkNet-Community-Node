// Package grant verifies private tracker grants.
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
)

var (
	ErrInvalid = errors.New("invalid grant")
	ErrExpired = errors.New("expired grant")
)

type Grant struct {
	ExpiresUnix int64
	Subject     [16]byte
	Hash        [32]byte
	Actions     byte
}

func Verify(token string, publicKey ed25519.PublicKey, now time.Time) (Grant, error) {
	var result Grant
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return result, ErrInvalid
	}
	encoding := base64.RawURLEncoding.Strict()
	payload, err := encoding.DecodeString(parts[0])
	if err != nil || len(payload) != PayloadSize {
		return result, ErrInvalid
	}
	signature, err := encoding.DecodeString(parts[1])
	if err != nil || len(signature) != ed25519.SignatureSize || len(publicKey) != ed25519.PublicKeySize {
		return result, ErrInvalid
	}
	if !ed25519.Verify(publicKey, payload, signature) || payload[0] != Version {
		return result, ErrInvalid
	}
	actions := payload[57]
	if actions == 0 || actions & ^(ActionDownload|ActionSeed) != 0 {
		return result, ErrInvalid
	}
	result.ExpiresUnix = int64(binary.BigEndian.Uint64(payload[1:9]))
	copy(result.Subject[:], payload[9:25])
	copy(result.Hash[:], payload[25:57])
	result.Actions = payload[57]
	if now.Unix() >= result.ExpiresUnix {
		return Grant{}, ErrExpired
	}
	return result, nil
}
