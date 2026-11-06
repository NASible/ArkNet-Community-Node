package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/core/internal/admins"
	"arknet/core/internal/media"
	"arknet/core/internal/nodes"
	"arknet/core/internal/relayrevocations"
	"arknet/core/internal/store"
	"arknet/core/internal/users"
)

// fakeAdmins is an in-memory admin authorizer: bearer value -> principal.
type fakeAdmins struct {
	mu         sync.Mutex
	principals map[string]admins.Principal
	audit      []admins.AuditEntry
	auditErr   error
}

func newFakeAdmins() *fakeAdmins { return &fakeAdmins{principals: map[string]admins.Principal{}} }

// grant returns a bearer header value for a principal holding permissions.
func (f *fakeAdmins) grant(permissions ...admins.Permission) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	token, _, _ := admins.NewToken()
	tokenID, _ := admins.NewTokenID()
	adminID, _ := admins.NewAdminID()
	f.principals[token] = admins.Principal{AdminID: adminID, AdminName: "test-admin", TokenID: tokenID, Permissions: permissions}
	return "Bearer " + token
}

func (f *fakeAdmins) Authenticate(r *http.Request) (admins.Principal, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return admins.Principal{}, admins.ErrNotFound
	}
	token, found := strings.CutPrefix(values[0], "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.principals[token]
	if !found || !ok {
		return admins.Principal{}, admins.ErrNotFound
	}
	return p, nil
}

func (f *fakeAdmins) AppendAudit(_ context.Context, entry admins.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audit = append(f.audit, entry)
	return nil
}

func (f *fakeAdmins) entries() []admins.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]admins.AuditEntry(nil), f.audit...)
}

// spyRepositories records every repository call reachable from admin routes.
// Embedded nil interfaces make any other call panic, which also fails a test.
type (
	spyUsers      = users.Repository
	spyDeliveries = relayrevocations.Repository
)

type spyRepositories struct {
	spyUsers
	store.MediaCatalogRepository
	spyDeliveries
	mu    sync.Mutex
	calls []string
}

func (s *spyRepositories) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
}
func (s *spyRepositories) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }
func (s *spyRepositories) ListPending(context.Context, string, int) ([]nodes.Node, error) {
	s.record("ListPending")
	return nil, nil
}
func (s *spyRepositories) Approve(_ context.Context, id string) (nodes.Node, error) {
	s.record("Approve")
	return nodes.Node{PublicID: id, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted}, nil
}
func (s *spyRepositories) Reject(_ context.Context, id string) (nodes.Node, error) {
	s.record("Reject")
	return nodes.Node{PublicID: id, EnrollmentStatus: nodes.EnrollmentRejected, TrustStatus: nodes.TrustBlocked}, nil
}
func (s *spyRepositories) Revoke(_ context.Context, id string) (nodes.Node, error) {
	s.record("Revoke")
	return nodes.Node{PublicID: id, EnrollmentStatus: nodes.EnrollmentRevoked, TrustStatus: nodes.TrustBlocked}, nil
}
func (s *spyRepositories) SetCapabilities(_ context.Context, id string, c []nodes.Capability) (nodes.Node, error) {
	s.record("SetCapabilities")
	return nodes.Node{PublicID: id, Capabilities: c}, nil
}
func (s *spyRepositories) GetByPublicID(context.Context, string) (nodes.Node, error) {
	s.record("GetByPublicID")
	return nodes.Node{}, nodes.ErrNotFound
}
func (s *spyRepositories) CreateUser(context.Context, string, string) (users.User, error) {
	s.record("CreateUser")
	return users.User{}, errors.New("spy")
}
func (s *spyRepositories) SetUserStatus(context.Context, string, users.Status) (users.User, error) {
	s.record("SetUserStatus")
	return users.User{}, users.ErrNotFound
}
func (s *spyRepositories) UpdateMediaObjectState(context.Context, string, media.ObjectState) (store.Object, error) {
	s.record("UpdateMediaObjectState")
	return store.Object{}, store.ErrNotFound
}
func (s *spyRepositories) PendingRelayRevocations(context.Context, []string, int) ([]relayrevocations.Task, error) {
	s.record("PendingRelayRevocations")
	return nil, nil
}

type adminRoute struct {
	method, path, body string
	permission         admins.Permission
	target             string
}

func adminRouteMatrix() []adminRoute {
	node := "node_" + strings.Repeat("a", 64)
	user := "user_" + strings.Repeat("b", 64)
	object := "media_" + strings.Repeat("c", 64)
	return []adminRoute{
		{"GET", "/api/admin/v1/nodes/pending", "", admins.NodesDecide, ""},
		{"GET", "/api/admin/v1/nodes/" + node, "", admins.NodesDecide, node},
		{"POST", "/api/admin/v1/nodes/" + node + "/approve", "", admins.NodesDecide, node},
		{"POST", "/api/admin/v1/nodes/" + node + "/reject", "", admins.NodesDecide, node},
		{"POST", "/api/admin/v1/nodes/" + node + "/revoke", "", admins.NodesDecide, node},
		{"PUT", "/api/admin/v1/nodes/" + node + "/capabilities", `{"capabilities":["swarm"]}`, admins.NodesDecide, node},
		{"POST", "/api/admin/v1/users", `{"username":"alice","password":"correct horse battery"}`, admins.UsersAdmin, ""},
		{"POST", "/api/admin/v1/users/" + user + "/disable", "", admins.UsersAdmin, user},
		{"POST", "/api/admin/v1/users/" + user + "/enable", "", admins.UsersAdmin, user},
		{"POST", "/api/admin/v1/media/" + object + "/remove", "", admins.MediaAdmin, object},
		{"GET", "/api/admin/v1/relays", "", admins.RelaysRead, ""},
	}
}

func scopedAdminHandler(t *testing.T, spy *spyRepositories, auth adminAuthorizer) http.Handler {
	t.Helper()
	return withRelays(withAdministration(newHandler(healthyDatabase{}), spy, auth), testRelays(t), nil, spy, auth, time.Now)
}

func serveAdmin(h http.Handler, route adminRoute, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	return response
}

func TestEveryAdminRouteRequiresItsOwnPermissionAndDeniedCallsNeverReachRepositories(t *testing.T) {
	for _, route := range adminRouteMatrix() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			for _, other := range admins.AllPermissions() {
				if other == route.permission {
					continue
				}
				spy, auth := &spyRepositories{}, newFakeAdmins()
				h := scopedAdminHandler(t, spy, auth)
				response := serveAdmin(h, route, auth.grant(other))
				if response.Code != http.StatusForbidden || spy.count() != 0 {
					t.Fatalf("with %s: status %d, repository calls %v", other, response.Code, spy.calls)
				}
				audit := auth.entries()
				if len(audit) != 1 || audit[0].Outcome != admins.OutcomeDenied || audit[0].Permission != route.permission ||
					audit[0].TokenID == "" || audit[0].AdminID == "" || audit[0].Target != route.target || audit[0].Status != 403 {
					t.Fatalf("denied audit = %+v", audit)
				}
			}
			// Every other permission together is still not enough.
			var rest []admins.Permission
			for _, p := range admins.AllPermissions() {
				if p != route.permission {
					rest = append(rest, p)
				}
			}
			spy, auth := &spyRepositories{}, newFakeAdmins()
			if response := serveAdmin(scopedAdminHandler(t, spy, auth), route, auth.grant(rest...)); response.Code != 403 || spy.count() != 0 {
				t.Fatalf("all other permissions: status %d, calls %v", response.Code, spy.calls)
			}
			// Positive control: the exact permission reaches the repository and is audited.
			spy, auth = &spyRepositories{}, newFakeAdmins()
			response := serveAdmin(scopedAdminHandler(t, spy, auth), route, auth.grant(route.permission))
			if response.Code == 401 || response.Code == 403 || spy.count() == 0 {
				t.Fatalf("granted: status %d, calls %v", response.Code, spy.calls)
			}
			audit := auth.entries()
			if len(audit) != 2 || audit[0].Outcome != admins.OutcomeAuthorized || audit[1].Outcome != admins.OutcomeCompleted ||
				audit[1].Status != response.Code || audit[0].Action != audit[1].Action || audit[0].Target != route.target ||
				!strings.HasPrefix(audit[0].Action, route.method+" /api/admin/v1/") {
				t.Fatalf("granted audit = %+v", audit)
			}
		})
	}
}

func TestUnauthenticatedAdminCallsAreRejectedBeforeRoutingWithoutAudit(t *testing.T) {
	valid, _, _ := admins.NewToken()
	for _, header := range []string{"", "Bearer", "Bearer " + valid, "Basic " + valid, "Bearer  " + valid,
		"Bearer " + strings.Repeat("a", 64)} {
		for _, route := range append(adminRouteMatrix(), adminRoute{method: "GET", path: "/api/admin/v1/unknown"}) {
			spy, auth := &spyRepositories{}, newFakeAdmins()
			response := serveAdmin(scopedAdminHandler(t, spy, auth), route, header)
			if response.Code != 401 || spy.count() != 0 || len(auth.entries()) != 0 {
				t.Fatalf("%q %s: status %d calls %v audit %d", header, route.path, response.Code, spy.calls, len(auth.entries()))
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("admin response cacheable")
			}
		}
	}
}

func TestAdminActionDoesNotRunWhenItCannotBeAudited(t *testing.T) {
	for _, route := range adminRouteMatrix() {
		spy, auth := &spyRepositories{}, newFakeAdmins()
		header := auth.grant(admins.AllPermissions()...)
		auth.auditErr = errors.New("audit store down")
		response := serveAdmin(scopedAdminHandler(t, spy, auth), route, header)
		if response.Code != http.StatusServiceUnavailable || spy.count() != 0 {
			t.Fatalf("%s: status %d calls %v", route.path, response.Code, spy.calls)
		}
	}
}

func TestAdminRoutePermissionRegistryIsExact(t *testing.T) {
	registered := map[string]admins.Permission{}
	routes := newAdminRoutes(http.NewServeMux(), newFakeAdmins())
	registerAdministrativeRoutes(routes, &spyRepositories{})
	for pattern, permission := range routes.permissions {
		registered[pattern] = permission
	}
	want := map[string]admins.Permission{
		"GET /api/admin/v1/nodes/pending":                admins.NodesDecide,
		"GET /api/admin/v1/nodes/{node_id}":              admins.NodesDecide,
		"POST /api/admin/v1/nodes/{node_id}/approve":     admins.NodesDecide,
		"POST /api/admin/v1/nodes/{node_id}/reject":      admins.NodesDecide,
		"POST /api/admin/v1/nodes/{node_id}/revoke":      admins.NodesDecide,
		"PUT /api/admin/v1/nodes/{node_id}/capabilities": admins.NodesDecide,
		"POST /api/admin/v1/users":                       admins.UsersAdmin,
		"POST /api/admin/v1/users/{user_id}/disable":     admins.UsersAdmin,
		"POST /api/admin/v1/users/{user_id}/enable":      admins.UsersAdmin,
		"POST /api/admin/v1/media/{id}/remove":           admins.MediaAdmin,
	}
	var got, expected []string
	for k, v := range registered {
		got = append(got, k+"="+string(v))
	}
	for k, v := range want {
		expected = append(expected, k+"="+string(v))
	}
	sort.Strings(got)
	sort.Strings(expected)
	if strings.Join(got, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("registered:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(expected, "\n"))
	}
}
