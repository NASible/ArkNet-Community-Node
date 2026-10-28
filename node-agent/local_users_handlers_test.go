package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localusers"
)

// memoryLocalUsers follows the localusers.Repository contract.
type memoryLocalUsers struct {
	mu       sync.Mutex
	byName   map[string]*memoryLocal
	byID     map[string]*memoryLocal
	sessions map[[32]byte]localusers.Session
	now      func() time.Time
}

type memoryLocal struct {
	user localusers.User
	hash string
}

func newMemoryLocalUsers() *memoryLocalUsers {
	return &memoryLocalUsers{byName: map[string]*memoryLocal{}, byID: map[string]*memoryLocal{}, sessions: map[[32]byte]localusers.Session{}, now: time.Now}
}

func (m *memoryLocalUsers) CreateLocalUser(_ context.Context, username, hash string) (localusers.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byName[username] != nil {
		return localusers.User{}, localusers.ErrUsernameTaken
	}
	id, _ := localusers.NewUserID()
	record := &memoryLocal{localusers.User{ID: id, Username: username, Status: localusers.StatusActive, CreatedAt: m.now()}, hash}
	m.byName[username], m.byID[id] = record, record
	return record.user, nil
}
func (m *memoryLocalUsers) ListLocalUsers(context.Context, int) ([]localusers.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	users := []localusers.User{}
	for _, record := range m.byName {
		users = append(users, record.user)
	}
	return users, nil
}
func (m *memoryLocalUsers) LocalCredentials(_ context.Context, username string) (localusers.Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.byName[username]
	if record == nil {
		return localusers.Credentials{}, localusers.ErrNotFound
	}
	return localusers.Credentials{UserID: record.user.ID, PasswordHash: record.hash, Status: record.user.Status}, nil
}
func (m *memoryLocalUsers) SetLocalUserStatus(_ context.Context, id string, status localusers.Status) (localusers.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.byID[id]
	if record == nil {
		return localusers.User{}, localusers.ErrNotFound
	}
	record.user.Status = status
	if status == localusers.StatusDisabled {
		for digest, session := range m.sessions {
			if session.UserID == id {
				delete(m.sessions, digest)
			}
		}
	}
	return record.user, nil
}
func (m *memoryLocalUsers) CreateLocalSession(_ context.Context, id string, digest [32]byte, ttl time.Duration) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.byID[id]
	if record == nil || record.user.Status != localusers.StatusActive {
		return time.Time{}, localusers.ErrInactive
	}
	expires := m.now().Add(ttl)
	m.sessions[digest] = localusers.Session{UserID: id, ExpiresAt: expires}
	return expires, nil
}
func (m *memoryLocalUsers) GetLocalSession(_ context.Context, digest [32]byte) (localusers.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[digest]
	if !ok || !m.now().Before(session.ExpiresAt) || m.byID[session.UserID].user.Status != localusers.StatusActive {
		return localusers.Session{}, localusers.ErrNotFound
	}
	return session, nil
}
func (m *memoryLocalUsers) DeleteLocalSession(_ context.Context, digest [32]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, digest)
	return nil
}

// recordingCore captures every request the Agent makes to Core.
type recordingCore struct {
	mu       sync.Mutex
	requests []string
	offline  bool
	members  string
}

func (c *recordingCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.String()+"\n"+string(body)+"\n"+r.Header.Get("Authorization"))
	offline, members := c.offline, c.members
	c.mu.Unlock()
	if offline {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, members)
}

type coreIdentityRepository struct {
	selectionRepository
	coreURL string
}

func (r *coreIdentityRepository) EnrollmentIdentity(context.Context) (enrollment.Identity, error) {
	return enrollment.Identity{NodeID: "node_" + strings.Repeat("a", 64), CoreURL: r.coreURL, Status: "APPROVED", PrivateSeed: make([]byte, ed25519.SeedSize)}, nil
}

const localPassword = "correct horse battery staple"

func localNode(t *testing.T) (http.Handler, *memoryLocalUsers, *recordingCore) {
	t.Helper()
	core := &recordingCore{members: `{"members":[{"member_id":"member_` + strings.Repeat("b", 64) + `","role":"MEMBER","joined_at":"2026-10-25T00:00:00Z"}],"truncated":false}`}
	server := httptest.NewServer(core)
	t.Cleanup(server.Close)
	repo := newMemoryLocalUsers()
	identity := &coreIdentityRepository{coreURL: server.URL}
	h := newHandler(testRepository{}, mediaOpt{repository: identity, coreURL: server.URL}, localUsersOpt{repository: repo})
	return h, repo, core
}

func localCall(t *testing.T, h http.Handler, method, path, body string, headers ...string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var decoded map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &decoded)
	return w, decoded
}

func credentials(username, password string) string {
	encoded, _ := json.Marshal(map[string]string{"username": username, "password": password})
	return string(encoded)
}

func TestLocalUserLifecycleWorksWithCoreOfflineAndNeverReachesCore(t *testing.T) {
	h, repo, core := localNode(t)
	core.offline = true

	w, created := localCall(t, h, "POST", "/api/v1/local-users", credentials("Alice", localPassword))
	if w.Code != http.StatusCreated || created["kind"] != "LOCAL" || created["username"] != "alice" || !localusers.ValidUserID(created["user_id"].(string)) {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	id := created["user_id"].(string)
	if strings.Contains(w.Body.String(), localPassword) || !strings.HasPrefix(repo.byName["alice"].hash, "$argon2id$") {
		t.Fatal("password leaked or stored unhashed")
	}
	if w, _ := localCall(t, h, "POST", "/api/v1/local-users", credentials("alice", localPassword)); w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", w.Code)
	}
	w, listed := localCall(t, h, "GET", "/api/v1/local-users", "")
	if w.Code != http.StatusOK || len(listed["users"].([]any)) != 1 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	w, session := localCall(t, h, "POST", "/api/v1/local-users/sessions", credentials("alice", localPassword))
	if w.Code != http.StatusCreated || session["user_id"] != id || session["kind"] != "LOCAL" {
		t.Fatalf("offline login: %d %s", w.Code, w.Body)
	}
	token := session["token"].(string)
	if w, _ := localCall(t, h, "GET", "/api/v1/local-users/session", "", "Authorization", "Bearer "+token); w.Code != http.StatusOK {
		t.Fatalf("session: %d", w.Code)
	}
	if w, _ := localCall(t, h, "GET", "/api/v1/global-members", ""); w.Code != http.StatusNotFound {
		t.Fatalf("global membership enumeration route: %d %s", w.Code, w.Body)
	}
	if w, _ := localCall(t, h, "POST", "/api/v1/local-users/"+id+"/disable", ""); w.Code != http.StatusOK {
		t.Fatalf("disable: %d", w.Code)
	}
	if w, _ := localCall(t, h, "GET", "/api/v1/local-users/session", "", "Authorization", "Bearer "+token); w.Code != http.StatusUnauthorized {
		t.Fatal("disabled local user's session survived")
	}
	if w, _ := localCall(t, h, "DELETE", "/api/v1/local-users/session", "", "Authorization", "Bearer "+token); w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", w.Code)
	}

	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.requests) != 0 {
		t.Fatalf("local user lifecycle contacted Core: %v", core.requests)
	}
}

func TestLocalLoginFailsIdenticallyAndGlobalMembersCannotBeEnumerated(t *testing.T) {
	h, _, _ := localNode(t)
	localCall(t, h, "POST", "/api/v1/local-users", credentials("alice", localPassword))
	var first string
	for _, attempt := range [][2]string{{"alice", localPassword + "!"}, {"nobody", localPassword}, {"al", localPassword}, {"alice", "short"}} {
		w, _ := localCall(t, h, "POST", "/api/v1/local-users/sessions", credentials(attempt[0], attempt[1]))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%v: %d", attempt, w.Code)
		}
		if first == "" {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Fatalf("distinguishable failure %q vs %q", w.Body.String(), first)
		}
	}
	if w, _ := localCall(t, h, "GET", "/api/v1/global-members", ""); w.Code != http.StatusNotFound {
		t.Fatalf("global membership enumeration route: %d %s", w.Code, w.Body)
	}
}

func TestLocalUserRequestsAreStrict(t *testing.T) {
	h, _, _ := localNode(t)
	for _, body := range []string{
		`{"username":"alice","password":"` + localPassword + `","kind":"GLOBAL"}`,
		`{"username":"alice","password":"` + localPassword + `","user_id":"user_` + strings.Repeat("0", 64) + `"}`,
		`{"username":"alice"}`,
		credentials("al", localPassword),
		credentials("alice", "short"),
	} {
		if w, _ := localCall(t, h, "POST", "/api/v1/local-users", body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/v1/local-users", strings.NewReader(credentials("alice", localPassword)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site create: %d", w.Code)
	}
	if w, _ := localCall(t, h, "POST", "/api/v1/local-users/user_"+strings.Repeat("0", 64)+"/disable", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("global ID accepted: %d", w.Code)
	}
}
