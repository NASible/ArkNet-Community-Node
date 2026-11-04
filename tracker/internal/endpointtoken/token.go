// Package endpointtoken verifies relay-issued endpoint capabilities. The
// wire format is shared with relay/internal/endpointtoken (golden tests).
package endpointtoken

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"
)

const Domain = "ARKNET-RELAY-ENDPOINT-V1\x00"
const PayloadSize = 63
const MaxTokenBytes = 256

var ErrInvalid = errors.New("invalid relay endpoint token")

type Claims struct {
	Endpoint    [6]byte // compact virtual IPv4 address and port, never an origin
	ExpiresUnix int64
}

// Verify binds the token to the grant's subject and full v2 info hash. Trust
// only operator-configured relay keys. A bearer token does not prove that
// the presenter owns a private key, nor that a registration is still live.
func Verify(token string, keys []ed25519.PublicKey, subject [16]byte, hash [32]byte, now time.Time) (Claims, error) {
	if len(token) > MaxTokenBytes {
		return Claims{}, ErrInvalid
	}
	encoded, signature, ok := strings.Cut(token, ".")
	if !ok || strings.Contains(signature, ".") {
		return Claims{}, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(payload) != PayloadSize || base64.RawURLEncoding.EncodeToString(payload) != encoded {
		return Claims{}, ErrInvalid
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(sig) != signature {
		return Claims{}, ErrInvalid
	}
	valid := false
	for _, key := range keys {
		if len(key) == ed25519.PublicKeySize && ed25519.Verify(key, append([]byte(Domain), payload...), sig) {
			valid = true
			break
		}
	}
	if !valid || payload[0] != 1 || string(payload[9:25]) != string(subject[:]) || string(payload[25:57]) != string(hash[:]) {
		return Claims{}, ErrInvalid
	}
	expires := int64(binary.BigEndian.Uint64(payload[1:9]))
	if now.Unix() >= expires || payload[57] != 100 || payload[58] < 64 || payload[58] > 127 || binary.BigEndian.Uint16(payload[61:63]) < 1024 {
		return Claims{}, ErrInvalid
	}
	result := Claims{ExpiresUnix: expires}
	copy(result.Endpoint[:], payload[57:63])
	return result, nil
}
