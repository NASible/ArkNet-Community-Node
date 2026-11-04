package endpointtoken

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestSignFieldsAndSignature(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	key := ed25519.NewKeyFromSeed(seed)
	var subject [16]byte
	copy(subject[:], bytes.Repeat([]byte{0xaa}, 16))
	var hash [32]byte
	copy(hash[:], bytes.Repeat([]byte{0xbb}, 32))
	token, err := Sign(key, "100.64.1.2:6881", subject, hash, time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Shared issuer/verifier golden vector; do not regenerate in the verifier test.
	const golden = "AQAAAABrSdIAqqqqqqqqqqqqqqqqqqqqqru7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7ZEABAhrh.FOi89iA21J1sH2JDh6Gn4HIgaCcCFatMvLSp4sWW3Fgcx_xbR8GjMx9YzS1rgz6dhTVu2kJUhZxXWFrASUidAQ"
	if token != golden {
		t.Fatal("shared golden vector changed")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		t.Fatal("invalid token format")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 63 || payload[0] != 1 || binary.BigEndian.Uint64(payload[1:9]) != 1800000000 ||
		!bytes.Equal(payload[9:25], subject[:]) || !bytes.Equal(payload[25:57], hash[:]) ||
		!bytes.Equal(payload[57:61], []byte{100, 64, 1, 2}) || binary.BigEndian.Uint16(payload[61:63]) != 6881 {
		t.Fatal("wrong payload")
	}
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), append([]byte("ARKNET-RELAY-ENDPOINT-V1\x00"), payload...), signature) {
		t.Fatal("wrong signature/domain")
	}
}
