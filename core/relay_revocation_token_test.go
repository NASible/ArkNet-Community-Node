package main

import (
	"bytes"
	"crypto/ed25519"
	"testing"
	"time"
)

// Same inputs and token as relay/internal/revocation/golden_test.go: Core's
// signer must stay byte-compatible with the relay's verifier.
const goldenRelayRevocation = "AQAAAABq6czAEREREREREREREREREREREenoCJLGZS1oIVi297PwLVyx8xN-9Q0sfk0MJSaiGgJGAAKqqqqqqqqqqqqqqqqqqqqqu7u7u7u7u7u7u7u7u7u7uw.ri84YAQzRWl8TXrPJd3uJkDDtTnZk6hzd3AF4s2L-B2zHUXlAVnDgCkqwDqKL5Xmtsw5EE_04SE6uPldMe3GAw"

func TestRelayRevocationTokenMatchesTheRelay(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	var id, a, b [16]byte
	copy(id[:], bytes.Repeat([]byte{0x11}, 16))
	copy(a[:], bytes.Repeat([]byte{0xaa}, 16))
	copy(b[:], bytes.Repeat([]byte{0xbb}, 16))
	token, err := signRelayRevocation(ed25519.NewKeyFromSeed(seed), "relay-golden", id,
		time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), [][16]byte{a, b})
	if err != nil || token != goldenRelayRevocation {
		t.Fatalf("%v\n%s", err, token)
	}
}

func TestRelayRevocationTokenBounds(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	if _, err := signRelayRevocation(key, "relay-1", [16]byte{}, time.Now(), nil); err == nil {
		t.Fatal("empty subject list signed")
	}
	if _, err := signRelayRevocation(key, "relay-1", [16]byte{}, time.Now(), make([][16]byte, maxRelayRevocationSubjects+1)); err == nil {
		t.Fatal("oversized subject list signed")
	}
	if _, err := signRelayRevocation(key, "", [16]byte{}, time.Now(), make([][16]byte, 1)); err == nil {
		t.Fatal("empty relay ID signed")
	}
}
