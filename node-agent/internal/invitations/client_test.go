package invitations

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/enrollment"
)

var nodeID = "node_" + strings.Repeat("a", 64)

func fakeCore(t *testing.T, status int, mutate func(map[string]any)) (*httptest.Server, ed25519.PublicKey, *[]byte) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = body
		message := fmt.Sprintf("ARKNET-NODE-V1\n%s\n%s\n%x\n%s\n%s", r.Method, r.URL.EscapedPath(), sha256.Sum256(body), r.Header.Get("X-ArkNet-Node-Timestamp"), nodeID)
		signature, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-ArkNet-Node-Signature"))
		if r.Method != "POST" || r.URL.Path != "/api/federation/v1/nodes/"+nodeID+"/invitations" || !ed25519.Verify(public, []byte(message), signature) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		response := map[string]any{
			"invitation_id": "invite_" + strings.Repeat("1", 64),
			"secret":        base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			"node_id":       nodeID, "role": request["role"], "max_uses": request["max_uses"], "expires_at": request["expires_at"],
		}
		if mutate != nil {
			mutate(response)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return server, public, &received
}

func identity(url string) enrollment.Identity {
	return enrollment.Identity{NodeID: nodeID, CoreURL: url, Status: "APPROVED", PrivateSeed: make([]byte, ed25519.SeedSize)}
}

func request() Request {
	return Request{Role: "MEMBER", ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second), MaxUses: 2}
}

func TestCreateSendsSignedUserlessPolicyAndValidatesReply(t *testing.T) {
	core, _, received := fakeCore(t, http.StatusCreated, nil)
	invitation, err := Create(context.Background(), identity(core.URL), request())
	if err != nil || invitation.NodeID != nodeID || invitation.MaxUses != 2 || len(invitation.Secret) != 43 {
		t.Fatalf("%+v %v", invitation, err)
	}
	var sent map[string]any
	if json.Unmarshal(*received, &sent) != nil || len(sent) != 3 || sent["role"] != "MEMBER" || sent["max_uses"] != float64(2) {
		t.Fatalf("sent %s", *received)
	}
}

func TestCreateMapsCoreRejectionsAndRejectsUnexpectedReplies(t *testing.T) {
	for status, want := range map[int]error{http.StatusBadRequest: ErrRejected, http.StatusForbidden: ErrNotApproved, http.StatusConflict: ErrRequestFailed, http.StatusOK: ErrRequestFailed} {
		core, _, _ := fakeCore(t, status, nil)
		if _, err := Create(context.Background(), identity(core.URL), request()); err != want {
			t.Fatalf("%d: %v", status, err)
		}
	}
	for name, mutate := range map[string]func(map[string]any){
		"other node":   func(m map[string]any) { m["node_id"] = "node_" + strings.Repeat("b", 64) },
		"other role":   func(m map[string]any) { m["role"] = "ADMIN" },
		"other uses":   func(m map[string]any) { m["max_uses"] = 100 },
		"short secret": func(m map[string]any) { m["secret"] = "abc" },
		"bad id":       func(m map[string]any) { m["invitation_id"] = "user_" + strings.Repeat("1", 64) },
		"extra field":  func(m map[string]any) { m["user_id"] = "user_" + strings.Repeat("1", 64) },
	} {
		core, _, _ := fakeCore(t, http.StatusCreated, mutate)
		if _, err := Create(context.Background(), identity(core.URL), request()); err != ErrRequestFailed {
			t.Fatalf("%s: %v", name, err)
		}
	}
	pending := identity("http://127.0.0.1:1")
	pending.Status = "PENDING"
	if _, err := Create(context.Background(), pending, request()); err != ErrRequestFailed {
		t.Fatalf("pending: %v", err)
	}
}
