package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/application"
	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/globalauth"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/localusers"
	"arknet/node-agent/internal/mediamiss"
	"arknet/node-agent/internal/operators"
	"arknet/node-agent/internal/transfer"
	"arknet/node-agent/internal/updates"
)

// Spies: every repository, service and probe behind the routes panics when
// called. A denied request must never reach one of them.
type spyState struct{ localstate.Repository }
type spyMedia struct{ mediaStorage }
type spySettings struct{ peerServingSettings }
type spyLocal struct{ localusers.Repository }
type spyGlobal struct{ globalauth.Repository }
type spyMisses struct{ mediamiss.Repository }
type spyUpdateSettings struct{ automaticUpdateSettings }
type spyTransfer struct{ transfer.Service }
type spyApplication struct{ application.Adapter }

type deniedOperatorRepository struct{ operators.Repository }

func (deniedOperatorRepository) CountActiveOperators(context.Context) (int, error) {
	panic("operator count read")
}

func (deniedOperatorRepository) GetOperatorSession(context.Context, [32]byte) (operators.Session, error) {
	panic("operator session read")
}

// allRouteOptions enables every optional route group, including diagnostics.
func allRouteOptions(accounts operators.Repository, now func() time.Time) (localstate.Repository, []handlerOption) {
	media := mediaOpt{repository: spyMedia{}, allocation: 1 << 30, coreURL: "https://core.invalid", mediaDir: "/nonexistent",
		fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) { panic("Core called") },
		grantTransfer: func(context.Context, enrollment.Identity, string) (catalog.TransferGrant, error) {
			panic("Core called")
		}}
	return spyState{}, []handlerOption{
		media, settingsOpt{spySettings{}}, localUsersOpt{repository: spyLocal{}},
		globalAuthOpt{repository: spyGlobal{}, identity: func(context.Context) (enrollment.Identity, error) { panic("identity read") }},
		mediaMissOpt{spyMisses{}}, updatesOpt{monitor: updates.NewMonitor(nil, nil), settings: spyUpdateSettings{}},
		transferOpt{service: spyTransfer{}}, applicationOpt{spyApplication{}},
		diagnosticsOpt{coreURL: "https://core.invalid", probeHTTP: func(context.Context, string) (string, int) { panic("probe") },
			probeTailscale: func(context.Context) bool { panic("probe") }, statfs: func(string) (uint64, uint64, error) { panic("statfs") }},
		operatorOpt{repository: accounts, now: now},
	}
}

func registeredPatterns(t *testing.T) []string {
	t.Helper()
	repo, opts := allRouteOptions(newMemoryOperators(time.Now), time.Now)
	patterns := append([]string(nil), newRoutes(repo, opts...).patterns...)
	sort.Strings(patterns)
	return patterns
}

func TestListenerPartitionsFollowRegisteredPermissionMatrix(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	accounts := newMemoryOperators(func() time.Time { return clock })
	repo, opts := allRouteOptions(accounts, func() time.Time { return clock })
	management, member := newListenerHandlers(repo, opts...)
	for _, pattern := range registeredPatterns(t) {
		permission := routePermissions[pattern]
		operatorRoute := permission == permOperator || permission == permStepUp
		right, wrong, rightName, wrongName := member, management, "member", "management"
		if operatorRoute {
			right, wrong, rightName, wrongName = management, member, "management", "member"
		}
		// Wrong listener: 404 before anything behind the partition runs.
		w, reached := serveSpy(wrong, requestFor(pattern))
		if reached != "" || w.Code != http.StatusNotFound {
			t.Errorf("%s on %s listener: HTTP %d, reached %q; want 404", pattern, wrongName, w.Code, reached)
		}
		// Right listener: the route is actually served. Without a session an
		// operator route stops at authorization (401); a public/member route
		// reaches its handler (a spy panics or the handler answers non-404).
		w, reached = serveSpy(right, requestFor(pattern))
		if operatorRoute && (reached != "" || w.Code != http.StatusUnauthorized) {
			t.Errorf("%s on %s listener: HTTP %d, reached %q; want 401 from authorization", pattern, rightName, w.Code, reached)
		}
		if !operatorRoute && reached == "" && w.Code == http.StatusNotFound {
			t.Errorf("%s is not served on the %s listener", pattern, rightName)
		}
	}
}

// Acceptance: generated from the registered routes. Adding a route without a
// matrix entry fails here (and is denied at runtime).
func TestEveryRegisteredRouteHasAPermission(t *testing.T) {
	patterns := registeredPatterns(t)
	seen := map[string]bool{}
	for _, pattern := range patterns {
		if seen[pattern] {
			t.Errorf("registered twice: %s", pattern)
		}
		seen[pattern] = true
		if _, ok := routePermissions[pattern]; !ok {
			t.Errorf("route %q has no entry in routePermissions (authz.go) or the docs matrix", pattern)
		}
	}
	for pattern := range routePermissions {
		if !seen[pattern] {
			t.Errorf("routePermissions lists %q, which is not registered", pattern)
		}
	}
	if len(patterns) != 34 {
		t.Errorf("registered %d routes; update the matrix and this count deliberately", len(patterns))
	}
}

// requestFor builds a concrete request for a route pattern.
func requestFor(pattern string) *http.Request {
	method, path, _ := strings.Cut(pattern, " ")
	path = strings.NewReplacer("{id}", "media_"+strings.Repeat("a", 64), "{action}", "start").Replace(path)
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// serve reports whether anything behind the middleware was called.
func serveSpy(h http.Handler, req *http.Request) (w *httptest.ResponseRecorder, reached string) {
	w = httptest.NewRecorder()
	func() {
		defer func() {
			if v := recover(); v != nil {
				reached = fmt.Sprint(v)
			}
		}()
		h.ServeHTTP(w, req)
	}()
	return w, reached
}

func TestAnonymousOperatorDenialWithoutRepository(t *testing.T) {
	mux := newRouteMux()
	mux.HandleFunc("GET /api/v1/status", func(http.ResponseWriter, *http.Request) { panic("handler reached") })
	w, reached := serveSpy(authorize(mux, nil, time.Now, nil), requestFor("GET /api/v1/status"))
	if w.Code != http.StatusUnauthorized || reached != "" {
		t.Fatalf("anonymous denial without repository: HTTP %d, reached %q", w.Code, reached)
	}
}

func TestAnonymousOperatorDenialDoesNotReadRepository(t *testing.T) {
	mux := newRouteMux()
	mux.HandleFunc("GET /api/v1/status", func(http.ResponseWriter, *http.Request) { panic("handler reached") })
	handler := authorize(mux, deniedOperatorRepository{}, time.Now, nil)
	w, reached := serveSpy(handler, requestFor("GET /api/v1/status"))
	if w.Code != http.StatusUnauthorized || reached != "" {
		t.Fatalf("anonymous denial read a repository or reached the handler: HTTP %d, reached %q", w.Code, reached)
	}
}

// Acceptance: anonymous, local-user and global-member callers get 401 or 403
// on every operator route, and no repository is called (spy test).
func TestOperatorRoutesDenyNonOperatorsBeforeAnyRepository(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	accounts := newMemoryOperators(func() time.Time { return clock })
	repo, opts := allRouteOptions(accounts, func() time.Time { return clock })
	handler := newHandler(repo, opts...)
	memberToken, _, _ := localusers.NewSessionToken()
	globalToken, _, _ := globalauth.NewSessionToken()

	// Before any operator exists the route is still closed without consulting a repository.
	w, reached := serveSpy(handler, requestFor("GET /api/v1/status"))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "operator_required") || reached != "" {
		t.Fatalf("no operator yet: %d %s %q", w.Code, w.Body, reached)
	}
	// Enroll a real operator so the denials below are not just "no operators".
	h := &operatorHarness{t: t, repo: accounts, clock: clock, handler: handler}
	_, operatorValue, _ := h.enroll(h.code("operator", "setup-code"), "alice")

	callers := map[string]func(*http.Request){
		"anonymous":              func(*http.Request) {},
		"local user bearer":      func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+memberToken) },
		"global member bearer":   func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+globalToken) },
		"member token as cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: operatorCookie, Value: memberToken}) },
		"forged operator cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "__Host-arknet_operator", Value: memberToken}) },
		"operator cookie plus bearer": func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "__Host-arknet_operator", Value: operatorValue})
			r.Header.Set("Authorization", "Bearer "+memberToken)
		},
		"operator cookie lookalike": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "arknet_operator", Value: operatorValue}) },
	}
	markerMux := newRouteMux()
	for _, pattern := range registeredPatterns(t) {
		markerMux.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) { panic("marker " + pattern) })
	}
	markers := authorize(markerMux, accounts, func() time.Time { return clock }, nil)
	checked := 0
	for _, pattern := range registeredPatterns(t) {
		perm := routePermissions[pattern]
		if perm != permOperator && perm != permStepUp {
			continue
		}
		for name, apply := range callers {
			req := requestFor(pattern)
			apply(req)
			w, reached := serveSpy(handler, req)
			if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
				t.Errorf("%s as %s: HTTP %d", pattern, name, w.Code)
			}
			if reached != "" {
				t.Errorf("%s as %s reached a repository or service: %s", pattern, name, reached)
			}
			checked++
		}
		// Positive control: with a valid operator session the middleware passes
		// the request on. Marker handlers stand in for the real ones here,
		// because some handlers call dependencies from goroutines.
		if pattern == "DELETE /api/v1/operator/session" {
			continue // logout would end the session used by the loop
		}
		req := requestFor(pattern)
		req.AddCookie(&http.Cookie{Name: "__Host-arknet_operator", Value: operatorValue})
		if w, reached := serveSpy(markers, req); reached != "marker "+pattern {
			t.Errorf("%s with a valid operator session was not passed on: %d %s", pattern, w.Code, w.Body)
		}
	}
	if checked != 24*len(callers) { // 24 operator routes in the matrix
		t.Fatalf("only %d denied requests checked", checked)
	}
}

func TestInvitationRequiresRecentSignIn(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	accounts := newMemoryOperators(func() time.Time { return clock })
	repo, opts := allRouteOptions(accounts, func() time.Time { return clock })
	handler := newHandler(repo, opts...)
	h := &operatorHarness{t: t, repo: accounts, clock: clock, handler: handler}
	_, cookie, _ := h.enroll(h.code("operator", "setup-code"), "alice")
	invite := func() (*httptest.ResponseRecorder, string) {
		req := requestFor("POST /api/v1/invitations")
		req.AddCookie(&http.Cookie{Name: "__Host-arknet_operator", Value: cookie})
		return serveSpy(handler, req)
	}
	if w, reached := invite(); w.Code != http.StatusBadRequest || reached != "" {
		t.Fatalf("fresh sign-in did not reach the invitation handler: HTTP %d, body %s, reached %q", w.Code, w.Body, reached)
	}
	// Keep the session alive past the step-up window.
	for range 3 {
		clock = clock.Add(4 * time.Minute)
		accounts.now = func() time.Time { return clock }
		req := requestFor("GET /api/v1/operator/session")
		req.AddCookie(&http.Cookie{Name: "__Host-arknet_operator", Value: cookie})
		if w, _ := serveSpy(handler, req); w.Code != http.StatusOK {
			t.Fatalf("session lapsed: %d", w.Code)
		}
	}
	w, reached := invite()
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "reauthentication_required") || reached != "" {
		t.Fatalf("stale sign-in created an invitation: %d %s %q", w.Code, w.Body, reached)
	}
}

// The design document's matrix and routePermissions must agree exactly, so the
// documented permissions are the enforced ones.
func TestDocumentedMatrixMatchesEnforcedPermissions(t *testing.T) {
	data, err := os.ReadFile("../docs/node-operator-authentication.md")
	if err != nil {
		t.Fatal(err)
	}
	documented, inTable := map[string]permission{}, false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "| Route |") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(line, "|")
		route, perm := strings.TrimSpace(cells[1]), strings.TrimSpace(cells[2])
		if strings.Trim(route, "-: ") == "" {
			continue
		}
		if !strings.HasPrefix(route, "`") || !strings.HasSuffix(route, "`") {
			t.Fatalf("matrix row without a `METHOD /path` route: %s", line)
		}
		documented[strings.Trim(route, "`")] = permission(perm)
	}
	if len(documented) == 0 {
		t.Fatal("no permission matrix found in the design document")
	}
	for route, perm := range routePermissions {
		if documented[route] != perm {
			t.Errorf("%s: enforced %q, documented %q", route, perm, documented[route])
		}
	}
	for route := range documented {
		if _, ok := routePermissions[route]; !ok {
			t.Errorf("documented route %s is not enforced", route)
		}
	}
}
