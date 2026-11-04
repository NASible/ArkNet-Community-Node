package revocation

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Unix(1_800_000_000, 0)

func subjects(n int) [][16]byte {
	out := make([][16]byte, n)
	for i := range out {
		out[i][0], out[i][15] = byte(i), byte(i>>8)
	}
	return out
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func mustSign(t *testing.T, key ed25519.PrivateKey, relayID string, id byte, issued time.Time, list [][16]byte) string {
	t.Helper()
	token, err := Sign(key, relayID, [16]byte{id}, issued, list)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestSignedRevocationVerifies(t *testing.T) {
	public, private := newKey(t)
	r, err := Verify(mustSign(t, private, "relay-1", 7, now, subjects(3)), public, "relay-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != [16]byte{7} || !r.IssuedAt.Equal(now) || len(r.Subjects) != 3 || r.Subjects[2] != subjects(3)[2] {
		t.Fatalf("%+v", r)
	}
}

func TestRevocationIsBoundToOneRelay(t *testing.T) {
	public, private := newKey(t)
	if _, err := Verify(mustSign(t, private, "relay-1", 1, now, subjects(1)), public, "relay-2", now); !errors.Is(err, ErrAudience) {
		t.Fatalf("revocation for another relay accepted: %v", err)
	}
}

func TestRevocationMustBeFresh(t *testing.T) {
	public, private := newKey(t)
	for name, issued := range map[string]time.Time{
		"too old":    now.Add(-MaxAge - time.Second),
		"from later": now.Add(ClockSkew + time.Second),
	} {
		if _, err := Verify(mustSign(t, private, "relay-1", 1, issued, subjects(1)), public, "relay-1", now); !errors.Is(err, ErrStale) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	for _, issued := range []time.Time{now.Add(-MaxAge), now.Add(ClockSkew)} {
		if _, err := Verify(mustSign(t, private, "relay-1", 1, issued, subjects(1)), public, "relay-1", now); err != nil {
			t.Errorf("boundary %v refused: %v", issued.Sub(now), err)
		}
	}
}

func TestUnsignedOrMalformedRevocationsAreRejected(t *testing.T) {
	public, private := newKey(t)
	_, other := newKey(t)
	good := mustSign(t, private, "relay-1", 1, now, subjects(2))
	payload, signature, _ := strings.Cut(good, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	countMismatch := append([]byte(nil), raw...)
	countMismatch[57]++ // claims one more subject than it carries
	signRaw := func(p []byte) string {
		return base64.RawURLEncoding.EncodeToString(p) + "." +
			base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, append([]byte(Domain), p...)))
	}
	withoutDomain := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, raw))
	trailing := append(append([]byte(nil), raw...), make([]byte, 16)...) // more subjects than counted
	badVersion := append([]byte(nil), raw...)
	badVersion[0] = 2
	for name, token := range map[string]string{
		"other key":      mustSign(t, other, "relay-1", 1, now, subjects(2)),
		"unsigned":       payload + ".",
		"no signature":   payload,
		"tampered":       base64.RawURLEncoding.EncodeToString(append(append([]byte(nil), raw[:len(raw)-1]...), raw[len(raw)-1]^1)) + "." + signature,
		"count mismatch": signRaw(countMismatch),
		"trailing bytes": signRaw(trailing),
		"no domain":      withoutDomain,
		"bad version":    signRaw(badVersion),
		"empty":          "",
	} {
		if _, err := Verify(token, public, "relay-1", now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSubjectCountIsBounded(t *testing.T) {
	_, private := newKey(t)
	if _, err := Sign(private, "relay-1", [16]byte{1}, now, nil); err == nil {
		t.Fatal("empty revocation signed")
	}
	if _, err := Sign(private, "relay-1", [16]byte{1}, now, subjects(MaxSubjects+1)); err == nil {
		t.Fatal("oversized revocation signed")
	}
	public, private := newKey(t)
	token := mustSign(t, private, "relay-1", 1, now, subjects(MaxSubjects))
	if len(token) > MaxTokenBytes {
		t.Fatalf("largest valid token (%d) exceeds MaxTokenBytes", len(token))
	}
	if _, err := Verify(token, public, "relay-1", now); err != nil {
		t.Fatal(err)
	}
}

func TestReplayGuardAcceptsEachRevocationOnce(t *testing.T) {
	guard := NewReplayGuard()
	r := Revocation{ID: [16]byte{1}, IssuedAt: now}
	if err := guard.Accept(r, now); err != nil {
		t.Fatal(err)
	}
	if err := guard.Accept(r, now.Add(time.Second)); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay accepted: %v", err)
	}
	if err := guard.Accept(Revocation{ID: [16]byte{2}, IssuedAt: now}, now); err != nil {
		t.Fatalf("a different revocation refused: %v", err)
	}
	// After the freshness window, Verify refuses the old message, so the guard may forget it.
	guard.Accept(Revocation{ID: [16]byte{3}, IssuedAt: now.Add(10 * time.Minute)}, now.Add(10*time.Minute))
	if guard.Len() != 1 {
		t.Fatalf("expired entries kept: %d", guard.Len())
	}
}
