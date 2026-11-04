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

var now = time.Unix(1_800_000_000, 0)

// sign builds a tracker-format grant (the format Core issues; see
// core/tracker_grants.go and docs/swarm-authorization-tokens.md).
func sign(t *testing.T, key ed25519.PrivateKey, expires time.Time, subject byte, hash byte, actions byte) string {
	t.Helper()
	payload := make([]byte, PayloadSize)
	payload[0] = Version
	binary.BigEndian.PutUint64(payload[1:9], uint64(expires.Unix()))
	for i := 9; i < 25; i++ {
		payload[i] = subject
	}
	for i := 25; i < 57; i++ {
		payload[i] = hash
	}
	payload[57] = actions
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
}

func keys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func TestVerifyReturnsSubjectHashActionsAndExpiry(t *testing.T) {
	public, private := keys(t)
	g, err := Verify(sign(t, private, now.Add(time.Minute), 0xaa, 0xbb, ActionSeed), public, ActionSeed, now)
	if err != nil {
		t.Fatal(err)
	}
	if g.Subject[0] != 0xaa || g.Subject[15] != 0xaa || g.Hash[0] != 0xbb || g.Hash[31] != 0xbb ||
		g.Actions != ActionSeed || !g.Expires.Equal(now.Add(time.Minute)) {
		t.Fatalf("%+v", g)
	}
}

func TestVerifyRequiresTheAction(t *testing.T) {
	public, private := keys(t)
	if _, err := Verify(sign(t, private, now.Add(time.Minute), 1, 2, ActionDownload), public, ActionSeed, now); !errors.Is(err, ErrAction) {
		t.Fatalf("download grant accepted for seeding: %v", err)
	}
	if _, err := Verify(sign(t, private, now.Add(time.Minute), 1, 2, ActionSeed), public, ActionDownload, now); !errors.Is(err, ErrAction) {
		t.Fatalf("seed grant accepted for downloading: %v", err)
	}
	if _, err := Verify(sign(t, private, now.Add(time.Minute), 1, 2, ActionSeed|ActionDownload), public, ActionDownload, now); err != nil {
		t.Fatalf("combined grant refused: %v", err)
	}
}

func TestVerifyExpiryIsExact(t *testing.T) {
	public, private := keys(t)
	token := sign(t, private, now, 1, 2, ActionSeed)
	if _, err := Verify(token, public, ActionSeed, now.Add(-time.Second)); err != nil {
		t.Fatalf("valid one second before expiry: %v", err)
	}
	if _, err := Verify(token, public, ActionSeed, now); !errors.Is(err, ErrExpired) {
		t.Fatalf("accepted at expiry: %v", err)
	}
}

func TestVerifyRejectsForgedOrMalformedGrants(t *testing.T) {
	public, private := keys(t)
	otherPublic, _ := keys(t)
	good := sign(t, private, now.Add(time.Minute), 1, 2, ActionSeed)
	payload, signature, _ := strings.Cut(good, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	tampered := append([]byte(nil), raw...)
	tampered[30] ^= 1 // another info hash
	badVersion := append([]byte(nil), raw...)
	badVersion[0] = 3
	unknownAction := append([]byte(nil), raw...)
	unknownAction[57] = 0x04
	reSign := func(p []byte) string {
		return base64.RawURLEncoding.EncodeToString(p) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, p))
	}
	for name, token := range map[string]string{
		"wrong key":       good,
		"tampered":        base64.RawURLEncoding.EncodeToString(tampered) + "." + signature,
		"bad version":     reSign(badVersion),
		"unknown action":  reSign(unknownAction),
		"no action":       reSign(append(append([]byte(nil), raw[:57]...), 0)),
		"short payload":   reSign(raw[:57]),
		"padded base64":   payload + "=." + signature,
		"three parts":     good + ".x",
		"empty":           "",
		"oversized token": strings.Repeat("A", MaxTokenBytes+1),
	} {
		key := public
		if name == "wrong key" {
			key = otherPublic
		}
		if _, err := Verify(token, key, ActionSeed, now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
