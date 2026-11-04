package relays

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/enrollment"
)

var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func sign(t *testing.T, key ed25519.PrivateKey, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte(Domain), raw...)))
}

func listPayload(issued, expires time.Time, relays ...map[string]string) map[string]any {
	if relays == nil {
		relays = []map[string]string{{"id": "relay-dc-1", "address": "relay1.example.net:7443", "spki_sha256": strings.Repeat("a", 64)}}
	}
	return map[string]any{"version": 1, "issued_at": issued.Unix(), "expires_at": expires.Unix(), "relays": relays}
}

func TestVerifyAcceptsCoresSignedList(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	list, err := Verify(sign(t, private, listPayload(now, now.Add(24*time.Hour))), public, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Relays) != 1 || list.Relays[0] != (Relay{ID: "relay-dc-1", Address: "relay1.example.net:7443", SPKISHA256: strings.Repeat("a", 64)}) ||
		!list.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("%+v", list)
	}
}

func TestVerifyRejectsForgedStaleOrMalformedLists(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	good := sign(t, private, listPayload(now, now.Add(time.Hour)))
	payload, signature, _ := strings.Cut(good, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	tampered := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), "relay1", "evil01", 1))) + "." + signature
	noDomain := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, raw))
	for name, token := range map[string]string{
		"other key":       sign(t, other, listPayload(now, now.Add(time.Hour))),
		"tampered":        tampered,
		"no domain":       noDomain,
		"expired":         sign(t, private, listPayload(now.Add(-2*time.Hour), now)),
		"from the future": sign(t, private, listPayload(now.Add(10*time.Minute), now.Add(time.Hour))),
		"too long-lived":  sign(t, private, listPayload(now, now.Add(8*24*time.Hour))),
		"wrong version":   sign(t, private, map[string]any{"version": 2, "issued_at": now.Unix(), "expires_at": now.Add(time.Hour).Unix(), "relays": []any{}}),
		"unknown field":   sign(t, private, map[string]any{"version": 1, "issued_at": now.Unix(), "expires_at": now.Add(time.Hour).Unix(), "relays": []any{}, "direct": true}),
		"bad address":     sign(t, private, listPayload(now, now.Add(time.Hour), map[string]string{"id": "r", "address": "relay", "spki_sha256": strings.Repeat("a", 64)})),
		"bad pin":         sign(t, private, listPayload(now, now.Add(time.Hour), map[string]string{"id": "r", "address": "r.example:1", "spki_sha256": "ab"})),
		"bad id":          sign(t, private, listPayload(now, now.Add(time.Hour), map[string]string{"id": "R 1", "address": "r.example:1", "spki_sha256": strings.Repeat("a", 64)})),
		"empty":           "",
	} {
		if _, err := Verify(token, public, now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func identityFor(t *testing.T, coreURL string) enrollment.Identity {
	t.Helper()
	seed := make([]byte, 32)
	return enrollment.Identity{Status: "APPROVED", CoreURL: coreURL, NodeID: "node_" + strings.Repeat("1", 64), PrivateSeed: seed}
}

func TestFetchUsesASignedFederationRequestAndVerifies(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	token := sign(t, private, listPayload(time.Now(), time.Now().Add(time.Hour)))
	var sawSignature bool
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSignature = r.Header.Get("X-ArkNet-Node-Signature") != ""
		if r.URL.Path != "/api/federation/v1/nodes/node_"+strings.Repeat("1", 64)+"/relays" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"relay_list": token})
	}))
	defer core.Close()
	list, err := Fetch(context.Background(), identityFor(t, core.URL), public)
	if err != nil || len(list.Relays) != 1 || !sawSignature {
		t.Fatalf("%+v %v signed=%v", list, err, sawSignature)
	}
	_, other, _ := ed25519.GenerateKey(nil)
	token = sign(t, other, listPayload(time.Now(), time.Now().Add(time.Hour)))
	if _, err := Fetch(context.Background(), identityFor(t, core.URL), public); !errors.Is(err, ErrInvalid) {
		t.Fatalf("list signed by another key accepted: %v", err)
	}
}

func TestHolderKeepsTheNewestValidListUntilItExpires(t *testing.T) {
	var h Holder
	if _, ok := h.Current(now); ok {
		t.Fatal("empty holder returned a list")
	}
	first := List{IssuedAt: now, ExpiresAt: now.Add(time.Hour), Relays: []Relay{{ID: "a"}}}
	older := List{IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(2 * time.Hour), Relays: []Relay{{ID: "old"}}}
	h.Store(first)
	h.Store(older) // a replayed older list never replaces a newer one
	if got, ok := h.Current(now); !ok || got.Relays[0].ID != "a" {
		t.Fatalf("%+v", got)
	}
	if _, ok := h.Current(now.Add(time.Hour)); ok {
		t.Fatal("expired list returned")
	}
}
