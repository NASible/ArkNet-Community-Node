package revocation

import (
	"bytes"
	"crypto/ed25519"
	"testing"
	"time"
)

// goldenToken is the token Core's signer (core/relay_revocation_token.go)
// must produce for these inputs; core has the same test.
const goldenToken = "AQAAAABq6czAEREREREREREREREREREREenoCJLGZS1oIVi297PwLVyx8xN-9Q0sfk0MJSaiGgJGAAKqqqqqqqqqqqqqqqqqqqqqu7u7u7u7u7u7u7u7u7u7uw.ri84YAQzRWl8TXrPJd3uJkDDtTnZk6hzd3AF4s2L-B2zHUXlAVnDgCkqwDqKL5Xmtsw5EE_04SE6uPldMe3GAw"

func TestGoldenTokenMatchesCore(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	key := ed25519.NewKeyFromSeed(seed)
	var id, a, b [16]byte
	copy(id[:], bytes.Repeat([]byte{0x11}, 16))
	copy(a[:], bytes.Repeat([]byte{0xaa}, 16))
	copy(b[:], bytes.Repeat([]byte{0xbb}, 16))
	issued := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	token, err := Sign(key, "relay-golden", id, issued, [][16]byte{a, b})
	if err != nil || token != goldenToken {
		t.Fatalf("token changed: %v\n%s", err, token)
	}
	r, err := Verify(goldenToken, key.Public().(ed25519.PublicKey), "relay-golden", issued)
	if err != nil || len(r.Subjects) != 2 || r.Subjects[1] != b {
		t.Fatalf("%+v %v", r, err)
	}
}
