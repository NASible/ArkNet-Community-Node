package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/invitations"
)

func invitationHandler(create invitationCreator) http.Handler {
	return newHandler(testRepository{}, mediaOpt{repository: &selectionRepository{}, coreURL: "https://core.example", createInvitation: create})
}

func TestOperatorCreatesInvitationThroughAgent(t *testing.T) {
	var got invitations.Request
	h := invitationHandler(func(_ context.Context, identity enrollment.Identity, request invitations.Request) (invitations.Invitation, error) {
		got = request
		return invitations.Invitation{InvitationID: "invite_" + strings.Repeat("1", 64), Secret: strings.Repeat("A", 43),
			NodeID: identity.NodeID, Role: request.Role, MaxUses: request.MaxUses, ExpiresAt: request.ExpiresAt}, nil
	})
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	body := `{"role":"ADMIN","expires_at":"` + expires.Format(time.RFC3339) + `","max_uses":3}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest("POST", "/api/v1/invitations", body))
	var response map[string]any
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response) != 5 || response["secret"] != strings.Repeat("A", 43) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got.Role != "ADMIN" || got.MaxUses != 3 || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("forwarded %+v", got)
	}
}

func TestAgentInvitationRejectsUserTargetingAndMapsCoreErrors(t *testing.T) {
	called := 0
	var result error
	h := invitationHandler(func(context.Context, enrollment.Identity, invitations.Request) (invitations.Invitation, error) {
		called++
		return invitations.Invitation{}, result
	})
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, body := range []string{
		`{"role":"MEMBER","expires_at":"` + expires + `","max_uses":1,"username":"alice"}`,
		`{"role":"MEMBER","expires_at":"` + expires + `"}`,
		`not json`,
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, selectionRequest("POST", "/api/v1/invitations", body))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	if called != 0 {
		t.Fatal("invalid local request reached Core")
	}
	body := `{"role":"MEMBER","expires_at":"` + expires + `","max_uses":1}`
	for err, want := range map[error]int{invitations.ErrRejected: 400, invitations.ErrNotApproved: 409, invitations.ErrRequestFailed: 503} {
		result = err
		w := httptest.NewRecorder()
		h.ServeHTTP(w, selectionRequest("POST", "/api/v1/invitations", body))
		if w.Code != want {
			t.Fatalf("%v: %d", err, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r := selectionRequest("POST", "/api/v1/invitations", body)
	r.Header.Set("Content-Type", "text/plain")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type: %d", w.Code)
	}
	w = httptest.NewRecorder()
	r = selectionRequest("POST", "/api/v1/invitations", body)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site request: %d", w.Code)
	}
}
