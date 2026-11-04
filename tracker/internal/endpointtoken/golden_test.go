package endpointtoken

import (
	"crypto/ed25519"
	"testing"
	"time"
)

// Identical to relay/internal/endpointtoken's issuance vector. The signer and
// verifier live in separate standard-library-only modules.
const goldenToken = "AQAAAABrSdIAqqqqqqqqqqqqqqqqqqqqqru7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7ZEABAhrh.FOi89iA21J1sH2JDh6Gn4HIgaCcCFatMvLSp4sWW3Fgcx_xbR8GjMx9YzS1rgz6dhTVu2kJUhZxXWFrASUidAQ"

func TestRelayGoldenToken(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	var subject [16]byte
	for i := range subject {
		subject[i] = 0xaa
	}
	var hash [32]byte
	for i := range hash {
		hash[i] = 0xbb
	}
	key := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	claims, err := Verify(goldenToken, []ed25519.PublicKey{key}, subject, hash, time.Unix(1799999999, 0))
	if err != nil || claims.Endpoint != [6]byte{100, 64, 1, 2, 0x1a, 0xe1} || claims.ExpiresUnix != 1800000000 {
		t.Fatalf("golden verification: %+v %v", claims, err)
	}
}
