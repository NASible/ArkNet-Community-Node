package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/core/internal/nodes"
	"arknet/core/internal/users"
)

// memoryUsers follows the users.Repository contract, including atomic active
// checks and disable-revokes-sessions, and also satisfies the node interfaces
// so requests flow through the real public and administrative wiring.
type memoryUsers struct {
	adminStoreStub
	mu         sync.Mutex
	byName     map[string]*memoryUser
	byID       map[string]*memoryUser
	sessions   map[[32]byte]memorySession
	now        func() time.Time
	failGet    error
	createHook func()
}

type memoryUser struct {
	user users.User
	hash string
}

type memorySession struct {
	userID    string
	expiresAt time.Time
}

func newMemoryUsers() *memoryUsers {
	return &memoryUsers{byName: map[string]*memoryUser{}, byID: map[string]*memoryUser{},
		sessions: map[[32]byte]memorySession{}, now: time.Now}
}

func (m *memoryUsers) Enroll(context.Context, string, string) (nodes.Node, error) {
	return nodes.Node{}, errors.New("unused")
}

func (m *memoryUsers) CreateUser(_ context.Context, username, hash string) (users.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byName[username] != nil {
		return users.User{}, users.ErrUsernameTaken
	}
	id, _ := users.NewUserID()
	record := &memoryUser{users.User{ID: id, Username: username, Status: users.StatusActive, CreatedAt: m.now()}, hash}
	m.byName[username], m.byID[id] = record, record
	return record.user, nil
}

func (m *memoryUsers) GetCredentials(_ context.Context, username string) (users.Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet != nil {
		return users.Credentials{}, m.failGet
	}
	record := m.byName[username]
	if record == nil {
		return users.Credentials{}, users.ErrNotFound
	}
	return users.Credentials{UserID: record.user.ID, PasswordHash: record.hash, Status: record.user.Status}, nil
}

func (m *memoryUsers) SetUserStatus(_ context.Context, id string, status users.Status) (users.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.byID[id]
	if record == nil {
		return users.User{}, users.ErrNotFound
	}
	record.user.Status = status
	if status == users.StatusDisabled {
		for digest, session := range m.sessions {
			if session.userID == id {
				delete(m.sessions, digest)
			}
		}
	}
	return record.user, nil
}

func (m *memoryUsers) CreateSession(_ context.Context, id string, digest [32]byte, ttl time.Duration) (time.Time, error) {
	if m.createHook != nil {
		m.createHook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.byID[id]
	if record == nil || record.user.Status != users.StatusActive {
		return time.Time{}, users.ErrInactive
	}
	expiresAt := m.now().Add(ttl)
	m.sessions[digest] = memorySession{id, expiresAt}
	return expiresAt, nil
}

func (m *memoryUsers) GetSession(_ context.Context, digest [32]byte) (users.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[digest]
	if !ok || !m.now().Before(session.expiresAt) || m.byID[session.userID].user.Status != users.StatusActive {
		return users.Session{}, users.ErrNotFound
	}
	return users.Session{UserID: session.userID, ExpiresAt: session.expiresAt}, nil
}

func (m *memoryUsers) DeleteSession(_ context.Context, digest [32]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, digest)
	return nil
}

func (m *memoryUsers) GetCredentialsByID(_ context.Context, id string) (users.Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.byID[id]
	if record == nil {
		return users.Credentials{}, users.ErrNotFound
	}
	return users.Credentials{UserID: id, PasswordHash: record.hash, Status: record.user.Status}, nil
}

func (m *memoryUsers) ReplacePassword(_ context.Context, id, currentHash, newHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.byID[id]
	if record == nil || record.hash != currentHash {
		return users.ErrNotFound
	}
	if record.user.Status != users.StatusActive {
		return users.ErrInactive
	}
	record.hash = newHash
	for digest, session := range m.sessions {
		if session.userID == id {
			delete(m.sessions, digest)
		}
	}
	return nil
}

const testPassword = "correct horse battery staple"

func identityServer(repo *memoryUsers, auth adminAuthorizer) http.Handler {
	return withAdministration(newHandler(healthyDatabase{}, repo), repo, auth)
}

func call(t *testing.T, h http.Handler, method, path, body string, headers ...string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Add(headers[i], headers[i+1])
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	var decoded map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response, decoded
}

func createUser(t *testing.T, h http.Handler, username string) string {
	t.Helper()
	response, body := call(t, h, "POST", "/api/admin/v1/users", `{"username":"`+username+`","password":"`+testPassword+`"}`, "Authorization", "Bearer x")
	if response.Code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", username, response.Code, response.Body.String())
	}
	return body["user_id"].(string)
}

func login(t *testing.T, h http.Handler, username, password string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	encoded, _ := json.Marshal(map[string]string{"username": username, "password": password})
	return call(t, h, "POST", "/api/identity/v1/sessions", string(encoded))
}

func TestAdminCreatesUserWithHashedPasswordOnly(t *testing.T) {
	repo := newMemoryUsers()
	h := identityServer(repo, allowAdmin(true))
	response, body := call(t, h, "POST", "/api/admin/v1/users", `{"username":"Alice","password":"`+testPassword+`"}`)
	if response.Code != http.StatusCreated || !users.ValidUserID(body["user_id"].(string)) || body["username"] != "alice" || body["status"] != "ACTIVE" {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), testPassword) || strings.Contains(response.Body.String(), "argon2") {
		t.Fatalf("response leaked secret material: %s", response.Body.String())
	}
	stored := repo.byName["alice"].hash
	if !strings.HasPrefix(stored, "$argon2id$") || strings.Contains(stored, testPassword) {
		t.Fatalf("stored %q", stored)
	}
	if response, _ := call(t, h, "POST", "/api/admin/v1/users", `{"username":"alice","password":"`+testPassword+`"}`); response.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", response.Code)
	}
	for _, body := range []string{
		`{"username":"al","password":"` + testPassword + `"}`,
		`{"username":"alice2","password":"short"}`,
		`{"username":"alice2"}`,
		`{"username":"alice2","password":"` + testPassword + `","status":"ACTIVE"}`,
		`{"username":"alice2","password":"` + testPassword + `","password":"x"}`,
		`{"username":7,"password":"` + testPassword + `"}`,
		`not json`,
	} {
		if response, _ := call(t, h, "POST", "/api/admin/v1/users", body); response.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, response.Code)
		}
	}
}

func TestUserAdministrationRequiresAdminAuthorization(t *testing.T) {
	repo := newMemoryUsers()
	for _, path := range []string{"/api/admin/v1/users", "/api/admin/v1/users/user_" + strings.Repeat("0", 64) + "/disable"} {
		response, _ := call(t, identityServer(repo, allowAdmin(false)), "POST", path, `{"username":"alice","password":"`+testPassword+`"}`)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d", path, response.Code)
		}
		if response, _ := call(t, identityServer(repo, nil), "POST", path, `{}`); response.Code != http.StatusNotFound {
			t.Fatalf("admin disabled %s: %d", path, response.Code)
		}
	}
	if len(repo.byName) != 0 {
		t.Fatal("unauthorized request created a user")
	}
	if response, _ := call(t, identityServer(repo, allowAdmin(true)), "GET", "/api/admin/v1/users", ""); response.Code == http.StatusOK {
		t.Fatal("global user directory is browsable")
	}
}

func TestValidLoginIssuesExpiringOpaqueSession(t *testing.T) {
	repo := newMemoryUsers()
	h := identityServer(repo, allowAdmin(true))
	id := createUser(t, h, "alice")
	response, body := login(t, h, "ALICE", testPassword)
	if response.Code != http.StatusCreated || body["user_id"] != id || body["token_type"] != "Bearer" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	token := body["token"].(string)
	digest, ok := users.SessionDigest(token)
	if !ok {
		t.Fatalf("token %q", token)
	}
	if _, stored := repo.sessions[digest]; !stored {
		t.Fatal("session digest not stored")
	}
	expiresAt, err := time.Parse(time.RFC3339, body["expires_at"].(string))
	if err != nil || time.Until(expiresAt) > users.SessionTTL || time.Until(expiresAt) < users.SessionTTL-time.Minute {
		t.Fatalf("expires_at %v %v", body["expires_at"], err)
	}
	response, body = call(t, h, "GET", "/api/identity/v1/session", "", "Authorization", "Bearer "+token)
	if response.Code != http.StatusOK || body["user_id"] != id {
		t.Fatalf("session: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "alice") {
		t.Fatal("session response exposes the username")
	}

	repo.now = func() time.Time { return time.Now().Add(users.SessionTTL + time.Second) }
	if response, _ := call(t, h, "GET", "/api/identity/v1/session", "", "Authorization", "Bearer "+token); response.Code != http.StatusUnauthorized {
		t.Fatalf("expired session accepted: %d", response.Code)
	}
	repo.now = time.Now

	if response, _ := call(t, h, "DELETE", "/api/identity/v1/session", "", "Authorization", "Bearer "+token); response.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", response.Code)
	}
	if response, _ := call(t, h, "GET", "/api/identity/v1/session", "", "Authorization", "Bearer "+token); response.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out session accepted: %d", response.Code)
	}
}

func TestInvalidCredentialsFailIdenticallyWithoutSession(t *testing.T) {
	repo := newMemoryUsers()
	h := identityServer(repo, allowAdmin(true))
	createUser(t, h, "alice")
	var first string
	for _, attempt := range [][2]string{
		{"alice", testPassword + "!"},
		{"nobody", testPassword},
		{"al", testPassword},
		{"alice", "short"},
		{"alice", strings.Repeat("p", users.MaxPasswordBytes+1)},
	} {
		response, _ := login(t, h, attempt[0], attempt[1])
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%q: %d", attempt[0], response.Code)
		}
		if first == "" {
			first = response.Body.String()
		} else if response.Body.String() != first {
			t.Fatalf("distinguishable failure: %q vs %q", response.Body.String(), first)
		}
	}
	if len(repo.sessions) != 0 {
		t.Fatal("failed login created a session")
	}
	repo.failGet = errors.New("database down")
	if response, _ := login(t, h, "alice", testPassword); response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "database") {
		t.Fatalf("store failure: %d %s", response.Code, response.Body.String())
	}
}

func TestDisabledUserCannotEstablishSessionsAndLosesExistingOnes(t *testing.T) {
	repo := newMemoryUsers()
	h := identityServer(repo, allowAdmin(true))
	id := createUser(t, h, "alice")
	_, body := login(t, h, "alice", testPassword)
	token := body["token"].(string)

	response, body := call(t, h, "POST", "/api/admin/v1/users/"+id+"/disable", "")
	if response.Code != http.StatusOK || body["status"] != "DISABLED" {
		t.Fatalf("disable: %d %s", response.Code, response.Body.String())
	}
	if response, _ := login(t, h, "alice", testPassword); response.Code != http.StatusUnauthorized || response.Body.String() != `{"error":"invalid credentials"}`+"\n" {
		t.Fatalf("disabled login: %d %s", response.Code, response.Body.String())
	}
	if response, _ := call(t, h, "GET", "/api/identity/v1/session", "", "Authorization", "Bearer "+token); response.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user's session accepted: %d", response.Code)
	}
	if len(repo.sessions) != 0 {
		t.Fatal("disabled user still holds sessions")
	}
	if response, _ := call(t, h, "POST", "/api/admin/v1/users/"+id+"/enable", ""); response.Code != http.StatusOK {
		t.Fatalf("enable: %d", response.Code)
	}
	if response, _ := login(t, h, "alice", testPassword); response.Code != http.StatusCreated {
		t.Fatalf("re-enabled login: %d", response.Code)
	}
	for path, want := range map[string]int{
		"/api/admin/v1/users/user_" + strings.Repeat("0", 64) + "/disable": http.StatusNotFound,
		"/api/admin/v1/users/node_" + strings.Repeat("0", 64) + "/disable": http.StatusBadRequest,
		"/api/admin/v1/users/" + id + "/disable?x=1":                       http.StatusBadRequest,
	} {
		if response, _ := call(t, h, "POST", path, ""); response.Code != want {
			t.Fatalf("%s: %d want %d", path, response.Code, want)
		}
	}
	if response, _ := call(t, h, "POST", "/api/admin/v1/users/"+id+"/disable", "{}"); response.Code != http.StatusBadRequest {
		t.Fatalf("non-empty action body: %d", response.Code)
	}
}

// Disabling between password verification and session creation still denies.
func TestDisableRacingLoginDeniesSession(t *testing.T) {
	repo := newMemoryUsers()
	h := identityServer(repo, allowAdmin(true))
	id := createUser(t, h, "alice")
	repo.createHook = func() { _, _ = repo.SetUserStatus(context.Background(), id, users.StatusDisabled) }
	if response, _ := login(t, h, "alice", testPassword); response.Code != http.StatusUnauthorized {
		t.Fatalf("raced login: %d", response.Code)
	}
	if len(repo.sessions) != 0 {
		t.Fatal("session created for disabled user")
	}
}

func TestSessionEndpointsRejectMalformedAuthorization(t *testing.T) {
	repo := newMemoryUsers()
	h := identityServer(repo, allowAdmin(true))
	token, _, _ := users.NewSessionToken()
	for _, headers := range [][]string{
		nil,
		{"Authorization", "Basic " + token},
		{"Authorization", "Bearer"},
		{"Authorization", "Bearer " + token + "x"},
		{"Authorization", "Bearer " + token, "Authorization", "Bearer " + token},
		{"Authorization", "Bearer " + token}, // well-formed but unknown
	} {
		response, _ := call(t, h, "GET", "/api/identity/v1/session", "", headers...)
		if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("%v: %d", headers, response.Code)
		}
	}
	if response, _ := call(t, h, "DELETE", "/api/identity/v1/session", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("logout without token: %d", response.Code)
	}
	if response, _ := call(t, h, "POST", "/api/identity/v1/sessions?x=1", `{"username":"alice","password":"`+testPassword+`"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("login query: %d", response.Code)
	}
	if response, _ := call(t, h, "POST", "/api/identity/v1/sessions", strings.Repeat("x", identityBodyLimit+1)); response.Code != http.StatusBadRequest {
		t.Fatalf("oversized login: %d", response.Code)
	}
}

func changePassword(t *testing.T, h http.Handler, token, current, replacement string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, _ := json.Marshal(map[string]string{"current_password": current, "new_password": replacement})
	response, _ := call(t, h, "POST", "/api/identity/v1/password", string(encoded), "Authorization", "Bearer "+token)
	return response
}

func TestPasswordChangeReplacesCredentialAndRevokesEverySession(t *testing.T) {
	repo := newMemoryUsers()
	h := identityServer(repo, allowAdmin(true))
	createUser(t, h, "alice")
	_, first := login(t, h, "alice", testPassword)
	_, second := login(t, h, "alice", testPassword)
	replacement := "a completely different passphrase"
	response := changePassword(t, h, first["token"].(string), testPassword, replacement)
	if response.Code != http.StatusNoContent || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("change: %d %s", response.Code, response.Body.String())
	}
	for _, token := range []string{first["token"].(string), second["token"].(string)} {
		if response, _ := call(t, h, "GET", "/api/identity/v1/session", "", "Authorization", "Bearer "+token); response.Code != http.StatusUnauthorized {
			t.Fatalf("session survived password change: %d", response.Code)
		}
	}
	if response, _ := login(t, h, "alice", testPassword); response.Code != http.StatusUnauthorized {
		t.Fatalf("old password still accepted: %d", response.Code)
	}
	if response, _ := login(t, h, "alice", replacement); response.Code != http.StatusCreated {
		t.Fatalf("new password rejected: %d", response.Code)
	}
	if strings.Contains(repo.byName["alice"].hash, replacement) || !strings.HasPrefix(repo.byName["alice"].hash, "$argon2id$") {
		t.Fatal("new password not stored as an argon2id hash")
	}
}

func TestPasswordChangeRequiresSessionAndCurrentPassword(t *testing.T) {
	repo := newMemoryUsers()
	h := identityServer(repo, allowAdmin(true))
	createUser(t, h, "alice")
	_, body := login(t, h, "alice", testPassword)
	token := body["token"].(string)
	original := repo.byName["alice"].hash
	replacement := "a completely different passphrase"
	unknown, _, _ := users.NewSessionToken()

	if response := changePassword(t, h, unknown, testPassword, replacement); response.Code != http.StatusUnauthorized {
		t.Fatalf("unknown session: %d", response.Code)
	}
	wrong := changePassword(t, h, token, testPassword+"!", replacement)
	failedLogin, _ := login(t, h, "alice", testPassword+"!")
	if wrong.Code != http.StatusUnauthorized || wrong.Body.String() != failedLogin.Body.String() {
		t.Fatalf("wrong current password: %d %q", wrong.Code, wrong.Body.String())
	}
	if response := changePassword(t, h, token, testPassword, "short"); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid new password: %d", response.Code)
	}
	if response := changePassword(t, h, token, testPassword, testPassword); response.Code != http.StatusBadRequest {
		t.Fatalf("unchanged password: %d", response.Code)
	}
	for _, payload := range []string{
		`{"current_password":"` + testPassword + `"}`,
		`{"current_password":"` + testPassword + `","new_password":"` + replacement + `","username":"bob"}`,
		`{"current_password":1,"new_password":"` + replacement + `"}`,
	} {
		if response, _ := call(t, h, "POST", "/api/identity/v1/password", payload, "Authorization", "Bearer "+token); response.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", payload, response.Code)
		}
	}
	if repo.byName["alice"].hash != original {
		t.Fatal("rejected request changed the stored password")
	}
	if response, _ := call(t, h, "GET", "/api/identity/v1/session", "", "Authorization", "Bearer "+token); response.Code != http.StatusOK {
		t.Fatalf("rejected change revoked the session: %d", response.Code)
	}
}
