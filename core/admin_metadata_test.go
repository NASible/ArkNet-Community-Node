package main

import (
	"arknet/core/internal/nodes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type metadataRepo struct {
	adminStoreStub
	node nodes.Node
}

func (s *metadataRepo) GetByPublicID(_ context.Context, id string) (nodes.Node, error) {
	s.calls++
	return s.node, s.err
}
func TestAdminNodeMetadataAndAuthorization(t *testing.T) {
	state := "CONNECTED"
	last := time.Now().UTC()
	for _, present := range []bool{false, true} {
		repo := &metadataRepo{node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted, SoftwareVersion: "v2"}}
		if present {
			repo.node.CurrentState = &state
			repo.node.LastSeenAt = &last
			repo.node.ReportedHealth = &nodes.BasicHealth{Healthy: true, DatabaseHealthy: false}
		}
		for _, auth := range []adminAuthorizer{nil, allowAdmin(false), allowAdmin(true)} {
			w := httptest.NewRecorder()
			before := repo.calls
			withAdministration(newHandler(nil), repo, auth).ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/v1/nodes/"+enrollmentID, nil))
			want := 200
			if auth == nil {
				want = 404
			} else if !auth.Authorized(nil) {
				want = 401
			}
			if w.Code != want {
				t.Fatalf("status %d want %d: %s", w.Code, want, w.Body)
			}
			if want != 200 {
				if repo.calls != before {
					t.Fatal("unauthorized read")
				}
				continue
			}
			var got map[string]json.RawMessage
			if json.Unmarshal(w.Body.Bytes(), &got) != nil {
				t.Fatal(w.Body)
			}
			n := 6
			if present {
				n = 7
			}
			if len(got) != n || string(got["node_id"]) != `"`+enrollmentID+`"` || string(got["version"]) != `"v2"` {
				t.Fatalf("metadata %s", w.Body)
			}
			if !present && (string(got["state"]) != "null" || string(got["last_seen_at"]) != "null") {
				t.Fatalf("nullable %s", w.Body)
			}
			if present && (string(got["state"]) != `"CONNECTED"` || string(got["health"]) != `{"healthy":true,"database_healthy":false}`) {
				t.Fatalf("reported %s", w.Body)
			}
		}
	}
}
func TestAdminNodeMetadataErrors(t *testing.T) {
	for _, tt := range []struct {
		suffix string
		err    error
		status int
	}{{"", nodes.ErrNotFound, 404}, {"", errors.New("secret password"), 503}, {"?x=1", nil, 400}, {"?", nil, 400}} {
		repo := &metadataRepo{adminStoreStub: adminStoreStub{err: tt.err}}
		w := httptest.NewRecorder()
		withAdministration(newHandler(nil), repo, allowAdmin(true)).ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/v1/nodes/"+enrollmentID+tt.suffix, nil))
		if w.Code != tt.status || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
}
