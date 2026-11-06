package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/nodes"
)

func TestUnsignedEnrollmentNeverReachesStorage(t *testing.T) {
	called := false
	repo := enrollFunc(func(context.Context, string, string) (nodes.Node, error) { called = true; return nodes.Node{}, nil })
	r := httptest.NewRequest("POST", enrollmentPath, strings.NewReader(enrollmentBody()))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	newHandler(nil, repo).ServeHTTP(w, r)
	if w.Code != 400 || called {
		t.Fatalf("unsigned: status=%d storage=%v", w.Code, called)
	}
}

func TestStatusUnknownAndDecidedAreIndistinguishableWithoutPinnedKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, attacker, _ := ed25519.GenerateKey(rand.Reader)
	var expected string
	for _, state := range []nodes.EnrollmentStatus{"", nodes.EnrollmentPending, nodes.EnrollmentApproved, nodes.EnrollmentRejected, nodes.EnrollmentRevoked} {
		repo := &handshakeRepo{node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: state}, key: pub}
		if state == "" {
			repo.err = nodes.ErrNotFound
		}
		w := httptest.NewRecorder()
		h := newHandler(nil, repo)
		h.ServeHTTP(w, signedRequest("GET", "/api/federation/v1/nodes/"+enrollmentID+"/enrollment", "", enrollmentID, attacker, fmt.Sprint(time.Now().Unix())))
		got := fmt.Sprint(w.Code, w.Header(), w.Body.String())
		if w.Code != 401 || (expected != "" && got != expected) {
			t.Fatalf("state %q: %s", state, got)
		}
		expected = got
	}
}

func TestSignedEnrollmentDoesNotRevealExistingState(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body := fmt.Sprintf(`{"node_id":%q,"version":"v","capabilities":{},"public_key":%q}`, enrollmentID, base64.StdEncoding.EncodeToString(pub))
	var response string
	for _, err := range []error{nil, nodes.ErrEnrollmentConflict} {
		repo := &handshakeRepo{err: err}
		w := httptest.NewRecorder()
		newHandler(nil, repo).ServeHTTP(w, signedRequest("POST", enrollmentPath, body, enrollmentID, priv, fmt.Sprint(time.Now().Unix())))
		if w.Code != 200 {
			t.Fatalf("receipt: %d %s", w.Code, w.Body)
		}
		if response != "" && response != w.Body.String() {
			t.Fatal("state-dependent receipt")
		}
		response = w.Body.String()
	}
}
