package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/nodes"
)

var relayListNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func testRelays(t *testing.T) *relaysConfig {
	t.Helper()
	cfg := &relaysConfig{}
	_, cfg.listKey, _ = ed25519.GenerateKey(rand.Reader)
	for i, id := range []string{"relay-dc-1", "relay-west-1"} {
		_, key, _ := ed25519.GenerateKey(rand.Reader)
		control, _ := url.Parse(fmt.Sprintf("http://10.0.0.%d:7080", 5+i))
		cfg.entries = append(cfg.entries, relayEntry{ID: id, Address: fmt.Sprintf("relay%d.example.net:7443", i+1),
			SPKISHA256: strings.Repeat(string(rune('a'+i)), 64), ControlURL: control, revocationKey: key})
	}
	return cfg
}

// approvedNode returns a handshake repository holding one APPROVED/TRUSTED Node.
func approvedNode(t *testing.T) (*handshakeRepo, ed25519.PrivateKey) {
	t.Helper()
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	return &handshakeRepo{key: public, node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved,
		TrustStatus: nodes.TrustTrusted}}, private
}

func relayListPath(id string) string { return "/api/federation/v1/nodes/" + id + "/relays" }

func getRelayList(t *testing.T, h http.Handler, key ed25519.PrivateKey) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest("GET", relayListPath(enrollmentID), "", enrollmentID, key, fmt.Sprint(time.Now().Unix())))
	return w
}

// verifyRelayList is what a Node does: check the signature with the pinned
// relay-list key, then decode the payload.
func verifyRelayList(t *testing.T, token string, key ed25519.PublicKey) map[string]any {
	t.Helper()
	encodedPayload, encodedSignature, ok := strings.Cut(token, ".")
	payload, err1 := base64.RawURLEncoding.DecodeString(encodedPayload)
	signature, err2 := base64.RawURLEncoding.DecodeString(encodedSignature)
	if !ok || err1 != nil || err2 != nil || !ed25519.Verify(key, append([]byte(relayListDomain), payload...), signature) {
		t.Fatalf("relay list signature does not verify: %q", token)
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestApprovedNodeGetsASignedRelayList(t *testing.T) {
	relays := testRelays(t)
	repo, key := approvedNode(t)
	h := withRelays(http.NotFoundHandler(), relays, repo, nil, nil, func() time.Time { return relayListNow })
	w := getRelayList(t, h, key)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var body map[string]string
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body) != 1 {
		t.Fatalf("body: %s", w.Body)
	}
	list := verifyRelayList(t, body["relay_list"], relays.listKey.Public().(ed25519.PublicKey))
	if list["version"] != float64(1) || list["issued_at"] != float64(relayListNow.Unix()) ||
		list["expires_at"] != float64(relayListNow.Add(relayListLifetime).Unix()) {
		t.Fatalf("header: %v", list)
	}
	entries := list["relays"].([]any)
	first := entries[0].(map[string]any)
	if len(entries) != 2 || len(first) != 3 || first["id"] != "relay-dc-1" || first["address"] != "relay1.example.net:7443" ||
		first["spki_sha256"] != strings.Repeat("a", 64) || entries[1].(map[string]any)["id"] != "relay-west-1" {
		t.Fatalf("relays: %v", entries)
	}
	// Nodes never learn control URLs or keys.
	if strings.Contains(w.Body.String(), "10.0.0.5") || strings.Contains(string(mustDecodePayload(t, body["relay_list"])), "control") {
		t.Fatal("relay list exposes a control URL")
	}
}

func mustDecodePayload(t *testing.T, token string) []byte {
	payload, _, _ := strings.Cut(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRelayListSignatureCoversTheWholeList(t *testing.T) {
	relays := testRelays(t)
	repo, key := approvedNode(t)
	h := withRelays(http.NotFoundHandler(), relays, repo, nil, nil, func() time.Time { return relayListNow })
	var body map[string]string
	_ = json.Unmarshal(getRelayList(t, h, key).Body.Bytes(), &body)
	payload, signature, _ := strings.Cut(body["relay_list"], ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	tampered := strings.Replace(string(raw), "relay1.example.net", "evil1.example.net", 1)
	sig, _ := base64.RawURLEncoding.DecodeString(signature)
	if ed25519.Verify(relays.listKey.Public().(ed25519.PublicKey), append([]byte(relayListDomain), tampered...), sig) {
		t.Fatal("tampered list verified")
	}
	if ed25519.Verify(relays.listKey.Public().(ed25519.PublicKey), raw, sig) {
		t.Fatal("signature is not domain-separated")
	}
	for _, entry := range relays.entries {
		if ed25519.Verify(entry.revocationKey.Public().(ed25519.PublicKey), append([]byte(relayListDomain), raw...), sig) {
			t.Fatal("relay list signed with a revocation key")
		}
	}
}

func TestRelayListRequiresAnApprovedSignedNode(t *testing.T) {
	relays := testRelays(t)
	repo, key := approvedNode(t)
	h := withRelays(http.NotFoundHandler(), relays, repo, nil, nil, time.Now)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", relayListPath(enrollmentID), nil))
	if w.Code != 401 || strings.Contains(w.Body.String(), "relay") {
		t.Fatalf("unsigned: %d %s", w.Code, w.Body)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if w := getRelayList(t, h, other); w.Code != 401 {
		t.Fatalf("wrong key: %d", w.Code)
	}
	repo.node.EnrollmentStatus, repo.node.TrustStatus = nodes.EnrollmentRevoked, nodes.TrustBlocked
	if w := getRelayList(t, h, key); w.Code != 403 || strings.Contains(w.Body.String(), "relay1") {
		t.Fatalf("revoked Node: %d %s", w.Code, w.Body)
	}
}

func TestRelayRoutesAreAbsentWithoutRelays(t *testing.T) {
	repo, key := approvedNode(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418) })
	if w := getRelayList(t, withRelays(next, nil, repo, nil, nil, time.Now), key); w.Code != 418 {
		t.Fatalf("%d", w.Code)
	}
}
