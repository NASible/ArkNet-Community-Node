package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"arknet/core/internal/nodes"
)

type adminStoreStub struct {
	calls int
	rows  []nodes.Node
	err   error
	after string
	limit int
}

func (s *adminStoreStub) ListPending(_ context.Context, after string, limit int) ([]nodes.Node, error) {
	s.calls++
	s.after = after
	s.limit = limit
	return s.rows, s.err
}
func (s *adminStoreStub) Approve(_ context.Context, id string) (nodes.Node, error) {
	s.calls++
	return nodes.Node{PublicID: id, EnrollmentStatus: "APPROVED", TrustStatus: "TRUSTED"}, s.err
}
func (s *adminStoreStub) Revoke(_ context.Context, id string) (nodes.Node, error) {
	s.calls++
	return nodes.Node{PublicID: id, EnrollmentStatus: "REVOKED", TrustStatus: "BLOCKED"}, s.err
}
func (s *adminStoreStub) Reject(_ context.Context, id string) (nodes.Node, error) {
	s.calls++
	return nodes.Node{PublicID: id, EnrollmentStatus: "REJECTED", TrustStatus: "BLOCKED"}, s.err
}

type allowAdmin bool

func (a allowAdmin) Authorized(*http.Request) bool { return bool(a) }

func TestAdministrativeNamespaceAndAuthorization(t *testing.T) {
	for _, test := range []struct {
		name   string
		auth   adminAuthorizer
		path   string
		status int
	}{
		{"disabled", nil, "/api/admin/v1/nodes/pending", 404},
		{"denied", allowAdmin(false), "/api/admin/v1/nodes/pending", 401},
		{"unknown protected", allowAdmin(false), "/api/admin/v1/unknown", 401},
		{"separate namespace", allowAdmin(true), "/api/federation/v1/nodes/pending", 404},
		{"authorized", allowAdmin(true), "/api/admin/v1/nodes/pending", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &adminStoreStub{}
			handler := withAdministration(newHandler(healthyDatabase{}), repo, test.auth)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("GET", test.path, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d", response.Code)
			}
			if test.status != 200 && repo.calls != 0 {
				t.Fatal("unauthorized/unknown request accessed repository")
			}
		})
	}
}

func TestAdministrativeSafeActionErrors(t *testing.T) {
	id := "node_" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"success", nil, 200}, {"missing", nodes.ErrNotFound, 404},
		{"duplicate", nodes.ErrEnrollmentConflict, 409}, {"invalid", nodes.ErrInvalidPublicID, 400},
		{"database", errors.New("private database credentials must not appear"), 503},
	} {
		for _, action := range []string{"approve", "reject"} {
			t.Run(test.name+action, func(t *testing.T) {
				repo := &adminStoreStub{err: test.err}
				response := httptest.NewRecorder()
				withAdministration(newHandler(healthyDatabase{}), repo, allowAdmin(true)).ServeHTTP(response, httptest.NewRequest("POST", "/api/admin/v1/nodes/"+id+"/"+action, nil))
				if response.Code != test.status {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if strings.Contains(response.Body.String(), "credentials") {
					t.Fatal("driver details leaked")
				}
				if response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("administrative response can be cached")
				}
			})
		}
	}
}

func TestAdministrativeValidationAndMethods(t *testing.T) {
	id := "node_" + strings.Repeat("a", 64)
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/admin/v1/nodes/not-opaque/approve", "", 400},
		{"POST", "/api/admin/v1/nodes/" + id + "/approve", "{}", 400},
		{"POST", "/api/admin/v1/nodes/" + id + "/reject?reason=anything", "", 400},
		{"GET", "/api/admin/v1/nodes/" + id + "/approve", "", 405},
		{"GET", "/api/admin/v1/nodes/pending?limit=0", "", 400},
		{"GET", "/api/admin/v1/nodes/pending?limit=101", "", 400},
		{"GET", "/api/admin/v1/nodes/pending?limit=invalid", "", 400},
		{"GET", "/api/admin/v1/nodes/pending?cursor=invalid", "", 400},
		{"GET", "/api/admin/v1/nodes/pending?limit=1&limit=2", "", 400},
		{"GET", "/api/admin/v1/nodes/pending?token=anything", "", 400},
		{"GET", "/api/admin/v1/nodes/pending?bad=%zz", "", 400},
	} {
		repo := &adminStoreStub{}
		response := httptest.NewRecorder()
		withAdministration(newHandler(healthyDatabase{}), repo, allowAdmin(true)).ServeHTTP(response, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		if response.Code != test.status || repo.calls != 0 {
			t.Errorf("%s %s: status=%d calls=%d", test.method, test.path, response.Code, repo.calls)
		}
	}
}

func TestAdministrativePaginationAndPublicIsolation(t *testing.T) {
	ids := []string{"node_" + strings.Repeat("a", 64), "node_" + strings.Repeat("b", 64), "node_" + strings.Repeat("c", 64)}
	repo := &adminStoreStub{rows: []nodes.Node{{PublicID: ids[0], EnrollmentStatus: "PENDING", TrustStatus: "UNTRUSTED"}, {PublicID: ids[1], EnrollmentStatus: "PENDING", TrustStatus: "UNTRUSTED"}, {PublicID: ids[2], EnrollmentStatus: "PENDING", TrustStatus: "UNTRUSTED"}}}
	handler := withAdministration(newHandler(healthyDatabase{}), repo, allowAdmin(true))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/admin/v1/nodes/pending?limit=2&cursor="+ids[0], nil))
	var page struct {
		Nodes []map[string]any `json:"nodes"`
		Next  string           `json:"next_cursor"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Nodes) != 2 || page.Next != ids[1] || repo.after != ids[0] || repo.limit != 2 {
		t.Fatalf("bad pagination response: %s", response.Body.String())
	}
	for _, node := range page.Nodes {
		if _, ok := node["id"]; ok {
			t.Fatal("internal numeric ID exposed")
		}
	}
	denied := withAdministration(newHandler(healthyDatabase{}), repo, allowAdmin(false))
	for _, path := range []string{"/health", "/api/federation/v1/version"} {
		response := httptest.NewRecorder()
		denied.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 {
			t.Fatal("administrative authorization changed public API")
		}
	}
	response = httptest.NewRecorder()
	withAdministration(newHandler(healthyDatabase{}), nil, allowAdmin(true)).ServeHTTP(response, httptest.NewRequest("GET", "/api/admin/v1/nodes/pending", nil))
	if response.Code != 503 {
		t.Fatal("nil repository did not fail safely")
	}
}
