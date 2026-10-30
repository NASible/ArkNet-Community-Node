package main

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/node-agent/internal/application"
	"arknet/node-agent/internal/operators"
)

// memoryOperators follows the operators.Repository contract (single-use codes
// and second factors, revocation on recovery and disable, idle and absolute
// session expiry) so the HTTP and CLI paths run without PostgreSQL.
// internal/postgres/operators_test.go covers the real store.
type memoryOperators struct {
	testRepository
	mu       sync.Mutex
	now      func() time.Time
	ops      map[string]*memOperator
	codes    map[[32]byte]*memCode
	enroll   map[[32]byte]*memEnroll
	sessions map[[32]byte]*memSession
}

type memOperator struct {
	operators.Operator
	hash     string
	secret   []byte
	last     int64
	failures int
	retry    time.Time
	recovery map[[32]byte]bool
}

type memCode struct {
	purpose  operators.Purpose
	username string
	expires  time.Time
	used     bool
}

type memEnroll struct {
	purpose        operators.Purpose
	username, hash string
	secret         []byte
	expires        time.Time
}

type memSession struct {
	username        string
	auth, idle, exp time.Time
}

func newMemoryOperators(now func() time.Time) *memoryOperators {
	return &memoryOperators{now: now, ops: map[string]*memOperator{}, codes: map[[32]byte]*memCode{},
		enroll: map[[32]byte]*memEnroll{}, sessions: map[[32]byte]*memSession{}}
}

func (m *memoryOperators) CountActiveOperators(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, op := range m.ops {
		if op.Status == operators.StatusActive {
			n++
		}
	}
	return n, nil
}

func (m *memoryOperators) IssueSetupCode(ctx context.Context, digest [32]byte, additional bool) (time.Time, error) {
	if n, _ := m.CountActiveOperators(ctx); n > 0 && !additional {
		return time.Time{}, operators.ErrOperatorsExist
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for d, c := range m.codes {
		if c.purpose == operators.PurposeSetup && !c.used {
			delete(m.codes, d)
		}
	}
	expires := m.now().Add(operators.SetupCodeTTL)
	m.codes[digest] = &memCode{purpose: operators.PurposeSetup, expires: expires}
	return expires, nil
}

func (m *memoryOperators) revoke(username string) {
	for d, s := range m.sessions {
		if s.username == username {
			delete(m.sessions, d)
		}
	}
}

func (m *memoryOperators) IssueRecoveryCode(_ context.Context, username string, digest [32]byte) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ops[username] == nil {
		return time.Time{}, operators.ErrNotFound
	}
	m.revoke(username)
	expires := m.now().Add(operators.SetupCodeTTL)
	m.codes[digest] = &memCode{purpose: operators.PurposeRecovery, username: username, expires: expires}
	return expires, nil
}

func (m *memoryOperators) BeginEnrollment(_ context.Context, code [32]byte, username, hash string, secret []byte, digest [32]byte) (operators.Enrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.codes[code]
	if c == nil || c.used || !m.now().Before(c.expires) || c.purpose == operators.PurposeRecovery && c.username != username {
		return operators.Enrollment{}, operators.ErrInvalidCode
	}
	if c.purpose == operators.PurposeSetup && m.ops[username] != nil {
		return operators.Enrollment{}, operators.ErrUsernameTaken
	}
	c.used = true
	e := &memEnroll{c.purpose, username, hash, secret, m.now().Add(operators.EnrollmentTTL)}
	m.enroll[digest] = e
	return operators.Enrollment{Purpose: e.purpose, Username: username, TOTPSecret: secret, ExpiresAt: e.expires}, nil
}

func (m *memoryOperators) PendingEnrollment(_ context.Context, digest [32]byte) (operators.Enrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.enroll[digest]
	if e == nil || !m.now().Before(e.expires) {
		return operators.Enrollment{}, operators.ErrInvalidCode
	}
	return operators.Enrollment{Purpose: e.purpose, Username: e.username, TOTPSecret: e.secret, ExpiresAt: e.expires}, nil
}

func (m *memoryOperators) newSession(op *memOperator, digest [32]byte) operators.Session {
	now := m.now()
	op.LastSignInAt, op.failures, op.retry = &now, 0, time.Time{}
	s := &memSession{op.Username, now, now.Add(operators.SessionIdleTTL), now.Add(operators.SessionTTL)}
	m.sessions[digest] = s
	return operators.Session{OperatorID: op.ID, Username: op.Username, AuthenticatedAt: s.auth, IdleExpiresAt: s.idle, ExpiresAt: s.exp}
}

func (m *memoryOperators) ConfirmEnrollment(_ context.Context, digest [32]byte, step int64, recovery [][32]byte, session [32]byte) (operators.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.enroll[digest]
	if e == nil || !m.now().Before(e.expires) {
		return operators.Session{}, operators.ErrInvalidCode
	}
	delete(m.enroll, digest)
	op := m.ops[e.username]
	if e.purpose == operators.PurposeSetup {
		id, _ := operators.NewOperatorID()
		op = &memOperator{Operator: operators.Operator{ID: id, Username: e.username, CreatedAt: m.now()}}
		m.ops[e.username] = op
	}
	m.revoke(e.username)
	op.Status, op.hash, op.secret, op.last, op.recovery = operators.StatusActive, e.hash, e.secret, step, map[[32]byte]bool{}
	for _, d := range recovery {
		op.recovery[d] = true
	}
	return m.newSession(op, session), nil
}

func (m *memoryOperators) OperatorCredentials(_ context.Context, username string) (operators.Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.ops[username]
	if op == nil {
		return operators.Credentials{}, operators.ErrNotFound
	}
	c := operators.Credentials{OperatorID: op.ID, Username: username, PasswordHash: op.hash, TOTPSecret: op.secret, TOTPLastStep: op.last, Status: op.Status}
	if op.retry.After(m.now()) {
		c.RetryAfter = &op.retry
	}
	return c, nil
}

func (m *memoryOperators) byID(id string) *memOperator {
	for _, op := range m.ops {
		if op.ID == id {
			return op
		}
	}
	return nil
}

func (m *memoryOperators) RecordFailedLogin(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.byID(id)
	if op == nil {
		return operators.ErrNotFound
	}
	op.failures++
	if d := operators.FailureDelay(op.failures); d > 0 {
		op.retry = m.now().Add(d)
	}
	return nil
}

func (m *memoryOperators) CreateOperatorSession(_ context.Context, id string, factor operators.SecondFactor, digest [32]byte) (operators.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.byID(id)
	if op == nil {
		return operators.Session{}, operators.ErrNotFound
	}
	if op.Status != operators.StatusActive {
		return operators.Session{}, operators.ErrInactive
	}
	if factor.TOTPStep > 0 {
		if factor.TOTPStep <= op.last {
			return operators.Session{}, operators.ErrReplay
		}
		op.last = factor.TOTPStep
	} else {
		if !op.recovery[*factor.RecoveryDigest] {
			return operators.Session{}, operators.ErrReplay
		}
		op.recovery[*factor.RecoveryDigest] = false
	}
	return m.newSession(op, digest), nil
}

func (m *memoryOperators) GetOperatorSession(_ context.Context, digest [32]byte) (operators.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[digest]
	now := m.now()
	if s == nil || !now.Before(s.idle) || !now.Before(s.exp) || m.ops[s.username].Status != operators.StatusActive {
		return operators.Session{}, operators.ErrNotFound
	}
	s.idle = now.Add(operators.SessionIdleTTL)
	if s.idle.After(s.exp) {
		s.idle = s.exp
	}
	return operators.Session{OperatorID: m.ops[s.username].ID, Username: s.username, AuthenticatedAt: s.auth, IdleExpiresAt: s.idle, ExpiresAt: s.exp}, nil
}

func (m *memoryOperators) DeleteOperatorSession(_ context.Context, digest [32]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, digest)
	return nil
}

func (m *memoryOperators) ListOperators(context.Context) ([]operators.Operator, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := []operators.Operator{}
	for _, op := range m.ops {
		list = append(list, op.Operator)
	}
	return list, nil
}

func (m *memoryOperators) DisableOperator(_ context.Context, username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.ops[username]
	if op == nil {
		return operators.ErrNotFound
	}
	op.Status = operators.StatusDisabled
	m.revoke(username)
	return nil
}

// ---------------------------------------------------------------- harness

type operatorHarness struct {
	t       *testing.T
	repo    *memoryOperators
	clock   time.Time
	handler http.Handler
	logs    bytes.Buffer // server-side log capture
	cliLogs bytes.Buffer // CLI stderr log capture
}

func newOperatorHarness(t *testing.T) *operatorHarness {
	h := &operatorHarness{t: t, clock: time.Unix(1_800_000_000, 0)}
	h.repo = newMemoryOperators(func() time.Time { return h.clock })
	logger := slog.New(slog.NewJSONHandler(&h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.handler = newHandler(testRepository{}, operatorOpt{repository: h.repo, logger: logger, now: func() time.Time { return h.clock }})
	return h
}

func (h *operatorHarness) cli(args ...string) (string, int) {
	h.t.Helper()
	var out bytes.Buffer
	env := cliEnv{
		getenv: func(string) string { return "" }, stdout: &out,
		logger:       slog.New(slog.NewJSONHandler(&h.cliLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		open:         func(context.Context, config) (cliStore, func(), error) { return h.repo, func() {}, nil },
		agentHealthy: func(context.Context, config) bool { return true },
		jellyfin:     func(config) application.Adapter { return nil },
	}
	code := runCLI(context.Background(), args, env)
	return out.String(), code
}

var displayCode = regexp.MustCompile(`[A-Z2-7]{4}(?:-[A-Z2-7]{4}){7}`)

func (h *operatorHarness) code(args ...string) string {
	h.t.Helper()
	out, exit := h.cli(args...)
	found := displayCode.FindString(out)
	if exit != 0 || found == "" {
		h.t.Fatalf("%v: exit %d %q", args, exit, out)
	}
	return found
}

func (h *operatorHarness) call(method, path string, body any, cookie string, headers ...string) (*httptest.ResponseRecorder, map[string]any) {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: operatorCookie, Value: cookie})
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	var decoded map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &decoded)
	return w, decoded
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == operatorCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", operatorCookie, w.Header())
	return nil
}

func totpNow(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return operators.TOTPCode(raw, operators.TOTPStep(at))
}

const operatorPassword = "correct horse operator staple"

// enroll runs setup + confirm and returns the TOTP secret, session cookie and recovery codes.
func (h *operatorHarness) enroll(code, username string) (string, string, []string) {
	h.t.Helper()
	w, body := h.call("POST", "/api/v1/operator/setup", map[string]string{"setup_code": code, "username": username, "password": operatorPassword}, "")
	if w.Code != http.StatusCreated {
		h.t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	secret := body["totp_secret"].(string)
	w, body = h.call("POST", "/api/v1/operator/setup/confirm", map[string]string{"enrollment_token": body["enrollment_token"].(string), "totp": totpNow(h.t, secret, h.clock)}, "")
	if w.Code != http.StatusCreated {
		h.t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	var codes []string
	for _, c := range body["recovery_codes"].([]any) {
		codes = append(codes, c.(string))
	}
	return secret, sessionCookie(h.t, w).Value, codes
}

// ---------------------------------------------------------------- tests

func TestOperatorBootstrapIsSingleUseAndStartsCookieSession(t *testing.T) {
	h := newOperatorHarness(t)
	code := h.code("operator", "setup-code")
	if out, exit := h.cli("operator", "setup-code"); exit != 0 || displayCode.FindString(out) == code {
		t.Fatalf("second setup code: %d", exit)
	}
	// The first code was superseded by the second.
	if w, _ := h.call("POST", "/api/v1/operator/setup", map[string]string{"setup_code": code, "username": "alice", "password": operatorPassword}, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("superseded code: %d", w.Code)
	}
	code = h.code("operator", "setup-code")
	w, body := h.call("POST", "/api/v1/operator/setup", map[string]string{"setup_code": strings.ToLower(code), "username": "Alice", "password": operatorPassword}, "")
	if w.Code != http.StatusCreated || body["purpose"] != "SETUP" || body["username"] != "alice" || !strings.HasPrefix(body["totp_uri"].(string), "otpauth://totp/") {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	if w, _ := h.call("POST", "/api/v1/operator/setup", map[string]string{"setup_code": code, "username": "mallory", "password": operatorPassword}, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("setup code reused: %d", w.Code)
	}
	token, secret := body["enrollment_token"].(string), body["totp_secret"].(string)
	if w, _ := h.call("POST", "/api/v1/operator/setup/confirm", map[string]string{"enrollment_token": token, "totp": "000000"}, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong TOTP confirmed: %d", w.Code)
	}
	w, body = h.call("POST", "/api/v1/operator/setup/confirm", map[string]string{"enrollment_token": token, "totp": totpNow(t, secret, h.clock)}, "")
	if w.Code != http.StatusCreated || len(body["recovery_codes"].([]any)) != operators.RecoveryCodeCount {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	cookie := sessionCookie(t, w)
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("cookie flags %+v", cookie)
	}
	if strings.Contains(w.Body.String(), cookie.Value) {
		t.Fatal("session token returned in the body")
	}
	if w, _ := h.call("POST", "/api/v1/operator/setup/confirm", map[string]string{"enrollment_token": token, "totp": totpNow(t, secret, h.clock)}, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("enrollment reused: %d", w.Code)
	}
	if w, body := h.call("GET", "/api/v1/operator/session", nil, cookie.Value); w.Code != http.StatusOK || body["username"] != "alice" {
		t.Fatalf("session: %d %s", w.Code, w.Body)
	}
	if out, exit := h.cli("operator", "setup-code"); exit != 1 || displayCode.MatchString(out) {
		t.Fatalf("setup code issued while an operator exists: %d %q", exit, out)
	}
	if out, exit := h.cli("operator", "setup-code", "--if-none"); exit != 0 || displayCode.MatchString(out) || !strings.Contains(out, "exist") {
		t.Fatalf("--if-none with an operator: %d %q", exit, out)
	}
	if _, exit := h.cli("operator", "setup-code", "--additional"); exit != 0 {
		t.Fatal("--additional refused")
	}
}

func TestSetupCodeIfNonePrintsACodeOnlyWithoutOperators(t *testing.T) {
	h := newOperatorHarness(t)
	if code := h.code("operator", "setup-code", "--if-none"); code == "" {
		t.Fatal("no code on a Node without operators")
	}
}

func TestOperatorLoginVerifiesPasswordAndSecondFactorAndLogout(t *testing.T) {
	h := newOperatorHarness(t)
	secret, _, recovery := h.enroll(h.code("operator", "setup-code"), "alice")
	h.clock = h.clock.Add(31 * time.Second) // next TOTP step
	login := func(body map[string]string) (*httptest.ResponseRecorder, map[string]any) {
		return h.call("POST", "/api/v1/operator/sessions", body, "")
	}
	wrong, _ := login(map[string]string{"username": "alice", "password": operatorPassword + "!", "totp": totpNow(t, secret, h.clock)})
	unknown, _ := login(map[string]string{"username": "nobody", "password": operatorPassword, "totp": totpNow(t, secret, h.clock)})
	badFactor, _ := login(map[string]string{"username": "alice", "password": operatorPassword, "totp": "000000"})
	for _, w := range []*httptest.ResponseRecorder{wrong, unknown, badFactor} {
		if w.Code != http.StatusUnauthorized || w.Body.String() != unknown.Body.String() {
			t.Fatalf("non-uniform failure: %d %s", w.Code, w.Body)
		}
	}
	if w, _ := login(map[string]string{"username": "alice", "password": operatorPassword, "totp": "1", "recovery_code": recovery[0]}); w.Code != http.StatusBadRequest {
		t.Fatalf("two second factors: %d", w.Code)
	}
	w, _ := login(map[string]string{"username": "alice", "password": operatorPassword, "totp": totpNow(t, secret, h.clock)})
	if w.Code != http.StatusCreated {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	cookie := sessionCookie(t, w).Value
	if w, _ := login(map[string]string{"username": "alice", "password": operatorPassword, "totp": totpNow(t, secret, h.clock)}); w.Code != http.StatusUnauthorized {
		t.Fatalf("TOTP replay accepted: %d", w.Code)
	}
	if w, _ := login(map[string]string{"username": "alice", "password": operatorPassword, "recovery_code": strings.ToLower(recovery[1])}); w.Code != http.StatusCreated {
		t.Fatalf("recovery code login: %d", w.Code)
	}
	if w, _ := login(map[string]string{"username": "alice", "password": operatorPassword, "recovery_code": recovery[1]}); w.Code != http.StatusUnauthorized {
		t.Fatalf("recovery code reused: %d", w.Code)
	}
	// Operator auth reads only the cookie; a member bearer token never counts.
	if w, _ := h.call("GET", "/api/v1/operator/session", nil, cookie, "Authorization", "Bearer "+cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("cookie plus Authorization accepted: %d", w.Code)
	}
	if w, _ := h.call("GET", "/api/v1/operator/session", nil, "", "Authorization", "Bearer "+cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("bearer accepted as operator session: %d", w.Code)
	}
	if w, _ := h.call("DELETE", "/api/v1/operator/session", nil, cookie); w.Code != http.StatusNoContent || sessionCookie(t, w).MaxAge >= 0 {
		t.Fatalf("logout: %d", w.Code)
	}
	if w, _ := h.call("GET", "/api/v1/operator/session", nil, cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out session accepted: %d", w.Code)
	}
}

func TestOperatorSessionIdleAndAbsoluteExpiry(t *testing.T) {
	h := newOperatorHarness(t)
	secret, cookie, _ := h.enroll(h.code("operator", "setup-code"), "alice")
	h.clock = h.clock.Add(operators.SessionIdleTTL - time.Minute)
	if w, _ := h.call("GET", "/api/v1/operator/session", nil, cookie); w.Code != http.StatusOK {
		t.Fatalf("active session rejected: %d", w.Code)
	}
	h.clock = h.clock.Add(operators.SessionIdleTTL + time.Second)
	if w, _ := h.call("GET", "/api/v1/operator/session", nil, cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("idle session accepted: %d", w.Code)
	}
	w, _ := h.call("POST", "/api/v1/operator/sessions", map[string]string{"username": "alice", "password": operatorPassword, "totp": totpNow(t, secret, h.clock)}, "")
	cookie = sessionCookie(t, w).Value
	// Keep the session busy: idle never lapses, yet the absolute lifetime still ends it.
	for elapsed := time.Duration(0); elapsed < operators.SessionTTL; elapsed += 20 * time.Minute {
		h.clock = h.clock.Add(20 * time.Minute)
		w, _ := h.call("GET", "/api/v1/operator/session", nil, cookie)
		if elapsed+20*time.Minute < operators.SessionTTL && w.Code != http.StatusOK {
			t.Fatalf("busy session rejected after %v: %d", elapsed, w.Code)
		}
	}
	h.clock = h.clock.Add(time.Minute)
	if w, _ := h.call("GET", "/api/v1/operator/session", nil, cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("session outlived its absolute lifetime: %d", w.Code)
	}
}

func TestOperatorShellRecoveryAndDisable(t *testing.T) {
	h := newOperatorHarness(t)
	_, cookie, _ := h.enroll(h.code("operator", "setup-code"), "alice")
	if out, exit := h.cli("operator", "recover", "nobody"); exit != 1 || displayCode.MatchString(out) {
		t.Fatalf("recover unknown: %d %q", exit, out)
	}
	code := h.code("operator", "recover", "alice")
	if w, _ := h.call("GET", "/api/v1/operator/session", nil, cookie); w.Code != http.StatusUnauthorized {
		t.Fatal("recover did not revoke sessions immediately")
	}
	if _, exit := h.cli("operator", "setup-code", "--additional"); exit != 0 {
		t.Fatal()
	}
	if w, _ := h.call("POST", "/api/v1/operator/setup", map[string]string{"setup_code": code, "username": "bob", "password": operatorPassword}, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("recovery code redeemed for another username: %d", w.Code)
	}
	h.clock = h.clock.Add(time.Minute)
	secret, cookie, _ := h.enroll(code, "alice")
	h.clock = h.clock.Add(31 * time.Second)
	if w, _ := h.call("POST", "/api/v1/operator/sessions", map[string]string{"username": "alice", "password": operatorPassword, "totp": totpNow(t, secret, h.clock)}, ""); w.Code != http.StatusCreated {
		t.Fatalf("login after recovery: %d", w.Code)
	}
	if out, exit := h.cli("operator", "list"); exit != 0 || !strings.Contains(out, "alice") || !strings.Contains(out, "ACTIVE") || displayCode.MatchString(out) {
		t.Fatalf("list: %d %q", exit, out)
	}
	if _, exit := h.cli("operator", "disable", "alice"); exit != 0 {
		t.Fatal("disable failed")
	}
	if w, _ := h.call("GET", "/api/v1/operator/session", nil, cookie); w.Code != http.StatusUnauthorized {
		t.Fatal("disable did not revoke sessions")
	}
	if _, exit := h.cli("operator", "bogus"); exit != 2 {
		t.Fatal("unknown subcommand accepted")
	}
}

func TestOperatorFailedAttemptsAreRateLimited(t *testing.T) {
	h := newOperatorHarness(t)
	h.enroll(h.code("operator", "setup-code"), "alice")
	for range maxOperatorFailuresPerMinute {
		h.call("POST", "/api/v1/operator/sessions", map[string]string{"username": "nobody", "password": operatorPassword, "totp": "000000"}, "")
	}
	if w, _ := h.call("POST", "/api/v1/operator/sessions", map[string]string{"username": "alice", "password": operatorPassword, "totp": "000000"}, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("not rate limited: %d", w.Code)
	}
	h.clock = h.clock.Add(time.Minute)
	if w, _ := h.call("POST", "/api/v1/operator/sessions", map[string]string{"username": "nobody", "password": operatorPassword, "totp": "000000"}, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("limit did not reset: %d", w.Code)
	}
}

// Acceptance: the bootstrap secret never appears in logs. Every code a CLI
// prints, and every recovery code the server returns, is searched for in the
// CLI's and the server's captured logs, including after failed redemptions.
func TestBootstrapAndRecoverySecretsNeverAppearInLogs(t *testing.T) {
	h := newOperatorHarness(t)
	setup := h.code("operator", "setup-code")
	// Failed redemptions log a rejection; the code must still be absent.
	h.call("POST", "/api/v1/operator/setup", map[string]string{"setup_code": setup + "X", "username": "alice", "password": operatorPassword}, "")
	secret, _, recovery := h.enroll(setup, "alice")
	h.call("POST", "/api/v1/operator/setup", map[string]string{"setup_code": setup, "username": "eve", "password": operatorPassword}, "")
	rec := h.code("operator", "recover", "alice")
	h.call("POST", "/api/v1/operator/setup", map[string]string{"setup_code": rec, "username": "eve", "password": operatorPassword}, "")
	h.cli("operator", "setup-code") // fails: operators exist
	logs := h.logs.String() + "\n" + h.cliLogs.String()
	if !strings.Contains(h.logs.String(), "operator_request_rejected") || !strings.Contains(h.cliLogs.String(), "cli_failed") {
		t.Fatalf("log capture is not wired: %q", logs)
	}
	secrets := []string{setup, strings.ReplaceAll(setup, "-", ""), rec, strings.ReplaceAll(rec, "-", ""), secret, operatorPassword}
	for _, code := range recovery {
		secrets = append(secrets, code, strings.ReplaceAll(code, "-", ""))
	}
	for _, value := range secrets {
		if strings.Contains(logs, value) || strings.Contains(strings.ToLower(logs), strings.ToLower(value)) {
			t.Fatal("a bootstrap, recovery or credential secret appeared in the logs")
		}
	}
}

func TestStatusCommandReportsLocalState(t *testing.T) {
	h := newOperatorHarness(t)
	h.repo.testRepository = testRepository{state: "CONNECTED", enrollmentState: "APPROVED"}
	out, exit := h.cli("status", "--json")
	var status map[string]any
	if exit != 0 || json.Unmarshal([]byte(out), &status) != nil {
		t.Fatalf("%d %q", exit, out)
	}
	if status["state"] != "CONNECTED" || status["agent_healthy"] != true || status["database_healthy"] != true || status["core_connectivity"] != "UNKNOWN" {
		t.Fatalf("status %v", status)
	}
	if _, exit := h.cli("status", "--yaml"); exit != 2 {
		t.Fatal("bad flag accepted")
	}
}

// The status CLI probes /health on the member listener: that is the only
// listener serving public routes after 45C, so probing management would
// report the Agent unhealthy and fail every upgrade.
func TestDefaultAgentHealthProbesMemberListener(t *testing.T) {
	management, member := newListenerHandlers(testRepository{})
	managementServer := httptest.NewServer(management)
	defer managementServer.Close()
	memberServer := httptest.NewServer(member)
	defer memberServer.Close()
	cfg := config{addr: strings.TrimPrefix(managementServer.URL, "http://"), memberAddr: strings.TrimPrefix(memberServer.URL, "http://")}
	env := defaultCLIEnv(func(string) string { return "" }, &bytes.Buffer{}, &bytes.Buffer{})
	if !env.agentHealthy(context.Background(), cfg) {
		t.Fatal("agent health probe did not reach the member listener's /health")
	}
}
