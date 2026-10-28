package grant

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

func signedToken(t *testing.T, privateKey ed25519.PrivateKey, expiry int64, version, actions byte) string {
	t.Helper()
	payload := make([]byte, PayloadSize)
	payload[0] = version
	binary.BigEndian.PutUint64(payload[1:9], uint64(expiry))
	payload[57] = actions
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
}

func TestVerifyRejectsMalformedAndUnauthorizedGrants(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherPublicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	valid := signedToken(t, privateKey, 2_000_000_000, Version, ActionDownload)
	cases := map[string]struct {
		token string
		key   ed25519.PublicKey
	}{
		"missing separator": {token: "not-a-grant", key: publicKey},
		"extra separator":   {token: valid + ".extra", key: publicKey},
		"bad base64":        {token: "!.!", key: publicKey},
		"wrong version":     {token: signedToken(t, privateKey, 2_000_000_000, 1, ActionDownload), key: publicKey},
		"wrong signer":      {token: valid, key: otherPublicKey},
		"short public key":  {token: valid, key: publicKey[:31]},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(testCase.token, testCase.key, time.Unix(1_900_000_000, 0)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Verify() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestVerifyRejectsNonCanonicalBase64URL(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	token := signedToken(t, privateKey, 2_000_000_000, Version, ActionDownload)
	parts := strings.Split(token, ".")
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, parts[1][len(parts[1])-1])
	if last < 0 || last%16 != 0 {
		t.Fatalf("unexpected canonical final character: %q", parts[1][len(parts[1])-1])
	}
	parts[1] = parts[1][:len(parts[1])-1] + string(alphabet[last+1])
	if _, err := Verify(strings.Join(parts, "."), publicKey, time.Unix(1_900_000_000, 0)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Verify() error = %v, want ErrInvalid for non-canonical base64url", err)
	}
}

func TestVerifyRejectsExpiredGrant(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	token := signedToken(t, privateKey, 2_000_000_000, Version, ActionDownload)
	if _, err := Verify(token, publicKey, time.Unix(2_000_000_000, 0)); !errors.Is(err, ErrExpired) {
		t.Fatalf("Verify() error = %v, want ErrExpired", err)
	}
}

func TestVerifyRejectsInvalidActionMasks(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, actions := range []byte{0, 0x04, ActionDownload | 0x80} {
		token := signedToken(t, privateKey, 2_000_000_000, Version, actions)
		if _, err := Verify(token, publicKey, time.Unix(1_900_000_000, 0)); !errors.Is(err, ErrInvalid) {
			t.Errorf("Verify(actions=%#x) error = %v, want ErrInvalid", actions, err)
		}
	}
}

func TestVerifyAcceptsCanonicalSignedGrant(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Unix(2_000_000_000, 0)
	payload := make([]byte, PayloadSize)
	payload[0] = Version
	binary.BigEndian.PutUint64(payload[1:9], uint64(expires.Unix()))
	for i := range 16 {
		payload[9+i] = byte(i + 1)
	}
	for i := range 32 {
		payload[25+i] = byte(32 - i)
	}
	payload[57] = ActionDownload | ActionSeed
	token := base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))

	got, err := Verify(token, publicKey, expires.Add(-time.Second))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.ExpiresUnix != expires.Unix() {
		t.Fatalf("ExpiresUnix = %d, want %d", got.ExpiresUnix, expires.Unix())
	}
	if got.Actions != ActionDownload|ActionSeed {
		t.Fatalf("Actions = %#x, want %#x", got.Actions, ActionDownload|ActionSeed)
	}
	for i := range got.Hash {
		if got.Hash[i] != byte(32-i) {
			t.Fatalf("Hash[%d] = %d", i, got.Hash[i])
		}
	}
}
