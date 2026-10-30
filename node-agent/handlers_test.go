package main

import (
	"arknet/node-agent/internal/application"
	"arknet/node-agent/internal/heartbeat"
	"arknet/node-agent/internal/localstate"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	response := httptest.NewRecorder()
	newRoutes(testRepository{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	checkJSON(t, response, `{"healthy":true,"database_healthy":true}`)
}

func TestInitialStatus(t *testing.T) {
	response := httptest.NewRecorder()
	newRoutes(testRepository{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	checkJSON(t, response, `{"version":"0.0.1-dev","state":"PROVISIONING","core_connectivity":"NOT_IMPLEMENTED","state_changed_at":"2026-01-01T00:00:00Z","node_id":"node_`+strings.Repeat("a", 64)+`","enrollment_state":"NOT_ENROLLED","last_successful_core_contact_at":null,"database_healthy":true,"agent_healthy":true,"jellyfin_healthy":null,"release":null}`)
	var got nodeStatus
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != stateProvisioning {
		t.Fatalf("state = %q", got.State)
	}
}

func checkJSON(t *testing.T, response *httptest.ResponseRecorder, expected string) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	var got, want map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected fields: %v", got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v", key, got[key], value)
		}
	}
}

func TestEndpointsRejectPost(t *testing.T) {
	for _, path := range []string{"/health", "/api/v1/status"} {
		response := httptest.NewRecorder()
		newRoutes(testRepository{}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d", path, response.Code)
		}
	}
}

func TestUnknownRoute(t *testing.T) {
	response := httptest.NewRecorder()
	newRoutes(testRepository{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestVersionMatchesStack(t *testing.T) {
	data, err := os.ReadFile("../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if version != strings.TrimSpace(string(data)) {
		t.Fatalf("agent version %q differs from VERSION", version)
	}
}

type testRepository struct {
	failure                   bool
	state                     localstate.State
	enrollmentState           string
	lastSuccessfulCoreContact time.Time
}

type testSettings struct {
	peerServing bool
	writes      int
}

func (s *testSettings) PeerServing(context.Context) (bool, error) { return s.peerServing, nil }
func (s *testSettings) SetPeerServing(_ context.Context, enabled bool) error {
	s.peerServing = enabled
	s.writes++
	return nil
}

func TestPeerServingSettingDefaultsOffAndCanBeChanged(t *testing.T) {
	settings := &testSettings{}
	handler := newRoutes(testRepository{}, settingsOpt{settings})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/settings/peer-serving", nil))
	checkJSON(t, w, `{"enabled":false}`)

	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/peer-serving", strings.NewReader(`{"enabled":true}`))
	r.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, r)
	checkJSON(t, w, `{"enabled":true}`)
	if !settings.peerServing || settings.writes != 1 {
		t.Fatalf("setting was not persisted: %+v", settings)
	}
}

func TestPeerServingSettingRejectsNonExactInput(t *testing.T) {
	for _, body := range []string{`{}`, `{"enabled":"true"}`, `{"enabled":true,"extra":false}`, `{"enabled":true}{}`} {
		settings := &testSettings{}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/peer-serving", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		newRoutes(testRepository{}, settingsOpt{settings}).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || settings.writes != 0 {
			t.Fatalf("body %q: status=%d writes=%d", body, w.Code, settings.writes)
		}
	}
}

func (s testRepository) Ping(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 2*time.Second {
		return errors.New("missing timeout")
	}
	if s.failure {
		return errors.New("SECRET database error")
	}
	return ctx.Err()
}
func (s testRepository) Snapshot(ctx context.Context) (localstate.Snapshot, error) {
	if s.failure {
		return localstate.Snapshot{}, errors.New("SECRET database error")
	}
	state := s.state
	if state == "" {
		state = localstate.Provisioning
	}
	enrollmentState := s.enrollmentState
	if enrollmentState == "" {
		enrollmentState = "NOT_ENROLLED"
	}
	return localstate.Snapshot{
		NodeID:                      "node_" + strings.Repeat("a", 64),
		State:                       state,
		StateChangedAt:              time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EnrollmentState:             enrollmentState,
		LastSuccessfulCoreContactAt: s.lastSuccessfulCoreContact,
	}, nil
}

func TestStatusExposesPersistedEnrollmentAndLastContact(t *testing.T) {
	contact := time.Date(2026, 2, 3, 4, 5, 6, 789000000, time.UTC)
	w := httptest.NewRecorder()
	newRoutes(testRepository{enrollmentState: "APPROVED", lastSuccessfulCoreContact: contact}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["enrollment_state"] != "APPROVED" || got["last_successful_core_contact_at"] != "2026-02-03T04:05:06.789Z" {
		t.Fatalf("unsafe or incomplete status: %v", got)
	}
	for _, forbidden := range []string{"core_url", "private_seed", "admin_token", "error"} {
		if _, exists := got[forbidden]; exists {
			t.Fatalf("status exposed %q", forbidden)
		}
	}
	// jellyfin_healthy should be present
	if _, exists := got["jellyfin_healthy"]; !exists {
		t.Fatalf("missing field: jellyfin_healthy")
	}
}

func TestDatabaseFailureHealthAndStatus(t *testing.T) {
	for _, repo := range []localstate.Repository{nil, testRepository{failure: true}} {
		for _, path := range []string{"/health", "/api/v1/status"} {
			w := httptest.NewRecorder()
			newRoutes(repo).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 503 || strings.Contains(w.Body.String(), "SECRET") {
				t.Fatalf("unsafe failure response: %d %s", w.Code, w.Body.String())
			}
			if path == "/health" && strings.TrimSpace(w.Body.String()) != `{"healthy":false,"database_healthy":false}` {
				t.Fatal(w.Body.String())
			}
		}
	}
}

func TestStatusUsesPersistedState(t *testing.T) {
	w := httptest.NewRecorder()
	newRoutes(testRepository{state: localstate.Connected}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	var got nodeStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != localstate.Connected {
		t.Fatal("state not loaded from database")
	}
}

func TestHealthCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	newRoutes(testRepository{}).ServeHTTP(w, httptest.NewRequest("GET", "/health", nil).WithContext(ctx))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestStatusExposesPersistedTransitionTimestamp(t *testing.T) {
	w := httptest.NewRecorder()
	newRoutes(testRepository{}).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/status", nil))
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["state_changed_at"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("timestamp = %v", got["state_changed_at"])
	}
}

func TestStatusWithMonitorAndJellyfin(t *testing.T) {
	monitor := heartbeat.NewMonitor()
	// Monitor defaults to "UNKNOWN" connectivity; verify monitor is still
	// wired through even when it has not yet recorded a state transition.
	w := httptest.NewRecorder()
	newRoutes(testRepository{}, monitorOpt{monitor}, applicationOpt{&testApplication{healthy: true}}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// connectivity defaults to NOT_IMPLEMENTED when no monitor state recorded.
	if got["jellyfin_healthy"] != true {
		t.Fatalf("jellyfin_healthy = %v", got["jellyfin_healthy"])
	}
}

func TestStatusReportsManagedApplicationUnhealthy(t *testing.T) {
	w := httptest.NewRecorder()
	newRoutes(testRepository{}, applicationOpt{&testApplication{healthy: false}}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["jellyfin_healthy"] != false {
		t.Fatalf("jellyfin_healthy = %v", got["jellyfin_healthy"])
	}
}

type testApplication struct {
	application.Adapter
	healthy bool
}

func (a *testApplication) Healthy(context.Context) bool { return a.healthy }

func TestJellyfinHealthyReturnsNilWhenNoClient(t *testing.T) {
	w := httptest.NewRecorder()
	newRoutes(testRepository{}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// When no Jellyfin is configured, jellyfin_healthy should be null.
	if got["jellyfin_healthy"] != nil {
		t.Fatalf("expected null jellyfin_healthy, got %v", got["jellyfin_healthy"])
	}
}
