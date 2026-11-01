package globalauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/enrollment"
)

func testIdentity(t *testing.T, server string) enrollment.Identity {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return enrollment.Identity{NodeID: "node_" + strings.Repeat("a", 64), CoreURL: server, Status: "APPROVED", PrivateSeed: key.Seed()}
}

func publishedKey(key ed25519.PrivateKey, status string) map[string]string {
	public := key.Public().(ed25519.PublicKey)
	return map[string]string{"key_id": KeyIDFor(public).String(), "algorithm": "Ed25519",
		"public_key": base64.RawURLEncoding.EncodeToString(public), "status": status}
}

func keyServer(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/federation/v1/assertion-keys" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFetchAssertionKeysAcceptsOnlyAWellFormedSetWithOneActiveKey(t *testing.T) {
	active, next := newTestKey(t), newTestKey(t)
	server := keyServer(t, 200, map[string]any{"keys": []any{publishedKey(active, "active"), publishedKey(next, "published")}})
	keys, err := FetchAssertionKeys(context.Background(), server.URL)
	if err != nil || len(keys) != 2 || keys[0].ID != KeyIDFor(active.Public().(ed25519.PublicKey)) || !keys[1].PublicKey.Equal(next.Public()) {
		t.Fatalf("keys: %+v %v", keys, err)
	}
	wrongID := publishedKey(active, "active")
	wrongID["key_id"] = strings.Repeat("0", 16)
	badKey := publishedKey(active, "active")
	badKey["public_key"] = "AAAA"
	for name, body := range map[string]any{
		"no keys":        map[string]any{"keys": []any{}},
		"no active key":  map[string]any{"keys": []any{publishedKey(next, "published")}},
		"two active":     map[string]any{"keys": []any{publishedKey(active, "active"), publishedKey(next, "active")}},
		"wrong key ID":   map[string]any{"keys": []any{wrongID}},
		"short key":      map[string]any{"keys": []any{badKey}},
		"duplicate":      map[string]any{"keys": []any{publishedKey(active, "active"), publishedKey(active, "published")}},
		"unknown status": map[string]any{"keys": []any{publishedKey(active, "active"), publishedKey(next, "retired")}},
		"extra field":    map[string]any{"keys": []any{publishedKey(active, "active")}, "x": 1},
	} {
		if _, err := FetchAssertionKeys(context.Background(), keyServer(t, 200, body).URL); !errors.Is(err, ErrInvalidResponse) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := FetchAssertionKeys(context.Background(), keyServer(t, 503, map[string]any{}).URL); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Core 5xx must be an outage: %v", err)
	}
	if _, err := FetchAssertionKeys(context.Background(), keyServer(t, 404, map[string]any{}).URL); !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("Core 404 must fail closed, not count as an outage: %v", err)
	}
	if _, err := FetchAssertionKeys(context.Background(), "http://127.0.0.1:1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("transport failure must be an outage: %v", err)
	}
}

func TestRegisterLoginCallbacksIsASignedPut(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/login-callbacks") || r.Header.Get("X-ArkNet-Node-Signature") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	urls := []string{"https://node.example:8443/login/callback"}
	if err := RegisterLoginCallbacks(context.Background(), testIdentity(t, server.URL), urls); err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"callback_urls":["https://node.example:8443/login/callback"]}` {
		t.Fatalf("body %s", body)
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer failing.Close()
	if err := RegisterLoginCallbacks(context.Background(), testIdentity(t, failing.URL), urls); err == nil {
		t.Fatal("refused registration reported success")
	}
}

func TestRefreshRequiresExactOrderedCompleteResponse(t *testing.T) {
	ids := []string{"member_" + strings.Repeat("d", 64), "member_" + strings.Repeat("e", 64)}
	now := time.Now().UTC().Truncate(time.Second)
	expires := now.Add(MaxOfflineWindow)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"authorizations": []any{map[string]any{"member_id": ids[0], "authorized": true, "role": "ADMIN", "permissions": []string{PermissionMediaAccess}, "validated_at": now.Format(time.RFC3339), "expires_at": expires.Format(time.RFC3339)}, map[string]any{"member_id": ids[1], "authorized": false}}})
		expires = now.Add(MaxOfflineWindow + time.Second)
	}))
	defer server.Close()
	states, err := Refresh(context.Background(), testIdentity(t, server.URL), ids)
	if err != nil || len(states) != 2 || !states[0].Authorized || states[1].Authorized {
		t.Fatalf("%+v %v", states, err)
	}
	if _, err := Refresh(context.Background(), testIdentity(t, server.URL), ids); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("refresh beyond the D3 window accepted: %v", err)
	}
}

func TestGrantBounds(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	base := Grant{MemberID: "member_" + strings.Repeat("f", 64), NodeID: "node_" + strings.Repeat("a", 64), Role: "MEMBER",
		IssuedAt: now, OfflineUntil: now.Add(MaxOfflineWindow), Assertion: "x.y"}
	if !ValidGrant(base, now) {
		t.Fatal("valid grant rejected")
	}
	for name, mutate := range map[string]func(*Grant){
		"beyond D3":   func(g *Grant) { g.OfflineUntil = g.IssuedAt.Add(MaxOfflineWindow + time.Second) },
		"expired":     func(g *Grant) { g.OfflineUntil = now },
		"no evidence": func(g *Grant) { g.Assertion = "" },
		"bad member":  func(g *Grant) { g.MemberID = "member_x" },
		"bad role":    func(g *Grant) { g.Role = "OWNER" },
	} {
		g := base
		mutate(&g)
		if ValidGrant(g, now) {
			t.Errorf("%s accepted", name)
		}
	}
}
