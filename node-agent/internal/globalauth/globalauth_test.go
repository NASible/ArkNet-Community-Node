package globalauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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

func TestAuthorizeSignsCredentialsAndValidatesBoundedMaterial(t *testing.T) {
	memberID := "member_" + strings.Repeat("b", 64)
	password := "correct global horse battery"
	var saw bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]string
		_ = json.Unmarshal(body, &request)
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/offline-authorizations") || r.Header.Get("X-ArkNet-Node-Signature") == "" || request["member_id"] != memberID || request["password"] != password {
			w.WriteHeader(400)
			return
		}
		saw = true
		now := time.Now().UTC().Truncate(time.Second)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"member_id": memberID, "node_id": "node_" + strings.Repeat("a", 64), "role": "MEMBER", "permissions": []string{PermissionMediaAccess}, "validated_at": now.Format(time.RFC3339), "expires_at": now.Add(time.Hour).Format(time.RFC3339)})
	}))
	defer server.Close()
	identity := testIdentity(t, server.URL)
	a, err := Authorize(context.Background(), identity, memberID, password)
	if err != nil || !saw || a.MemberID != memberID || a.PasswordHash != "" || !ValidPermissions(a.Permissions) {
		t.Fatalf("%+v %v saw=%v", a, err, saw)
	}
}

func TestAuthorizeClassifiesOnlyOutagesForFallback(t *testing.T) {
	memberID := "member_" + strings.Repeat("c", 64)
	for status, want := range map[int]error{401: ErrDenied, 403: ErrDenied, 500: ErrUnavailable, 429: ErrInvalidResponse, 200: ErrInvalidResponse} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status == 200 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"member_id":"bad"}`)
				return
			}
			w.WriteHeader(status)
		}))
		identity := testIdentity(t, server.URL)
		_, err := Authorize(context.Background(), identity, memberID, "correct global horse battery")
		server.Close()
		if err != want {
			t.Fatalf("status %d: %v want %v", status, err, want)
		}
	}
}

func TestRefreshRequiresExactOrderedCompleteResponse(t *testing.T) {
	ids := []string{"member_" + strings.Repeat("d", 64), "member_" + strings.Repeat("e", 64)}
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"authorizations": []any{map[string]any{"member_id": ids[0], "authorized": true, "role": "ADMIN", "permissions": []string{PermissionMediaAccess}, "validated_at": now.Format(time.RFC3339), "expires_at": now.Add(time.Hour).Format(time.RFC3339)}, map[string]any{"member_id": ids[1], "authorized": false}}})
	}))
	defer server.Close()
	states, err := Refresh(context.Background(), testIdentity(t, server.URL), ids)
	if err != nil || len(states) != 2 || !states[0].Authorized || states[1].Authorized {
		t.Fatalf("%+v %v", states, err)
	}
}

func TestNodeSpecificVerifierAndAuthorizationBounds(t *testing.T) {
	password := "same global password here"
	first, _ := HashPassword(context.Background(), password)
	second, _ := HashPassword(context.Background(), password)
	if first == second {
		t.Fatal("fresh Node verifiers are linkable")
	}
	for _, hash := range []string{first, second} {
		ok, err := VerifyPassword(context.Background(), hash, password)
		if err != nil || !ok {
			t.Fatal("verifier failed")
		}
	}
	now := time.Now()
	base := Authorization{MemberID: "member_" + strings.Repeat("f", 64), NodeID: "node_" + strings.Repeat("a", 64), PasswordHash: first, Role: "MEMBER", Permissions: []string{PermissionMediaAccess}, ValidatedAt: now, ExpiresAt: now.Add(MaxAuthorizationTTL)}
	if !ValidAuthorization(base, now) {
		t.Fatal("valid authorization rejected")
	}
	base.ExpiresAt = now.Add(MaxAuthorizationTTL + time.Second)
	if ValidAuthorization(base, now) {
		t.Fatal("overlong authorization accepted")
	}
	base.ExpiresAt = now
	if ValidAuthorization(base, now) {
		t.Fatal("expired authorization accepted")
	}
}
