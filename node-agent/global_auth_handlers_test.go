package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/globalauth"
)

const (
	testCallbackURL = "https://node.example:8443/login/callback"
	testCoreLogin   = "https://core.example"
)

var (
	thisNode    = "node_" + strings.Repeat("a", 64)
	foreignNode = "node_" + strings.Repeat("c", 64)
	aliceMember = "member_" + strings.Repeat("b", 64)
)

// memoryGlobalAuth mirrors the PostgreSQL store's contract for handler tests.
type memoryGlobalAuth struct {
	mu       sync.Mutex
	keys     []globalauth.VerificationKey
	pending  map[[32]byte][32]byte
	used     map[[16]byte]bool
	grants   map[string]globalauth.Grant
	devices  map[[32]byte]string
	sessions map[[32]byte]globalauth.Session
}

func newMemoryGlobalAuth() *memoryGlobalAuth {
	return &memoryGlobalAuth{pending: map[[32]byte][32]byte{}, used: map[[16]byte]bool{}, grants: map[string]globalauth.Grant{},
		devices: map[[32]byte]string{}, sessions: map[[32]byte]globalauth.Session{}}
}

func (m *memoryGlobalAuth) AssertionKeys(context.Context) ([]globalauth.VerificationKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]globalauth.VerificationKey(nil), m.keys...), nil
}
func (m *memoryGlobalAuth) ReplaceAssertionKeys(_ context.Context, keys []globalauth.VerificationKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys = append([]globalauth.VerificationKey(nil), keys...)
	return nil
}
func (m *memoryGlobalAuth) BeginLogin(_ context.Context, challenge, nonce [32]byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[challenge] = nonce
	return nil
}
func (m *memoryGlobalAuth) PendingLoginNonce(_ context.Context, challenge [32]byte) ([32]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nonce, ok := m.pending[challenge]
	if !ok {
		return nonce, globalauth.ErrNotFound
	}
	return nonce, nil
}
func (m *memoryGlobalAuth) CompleteLogin(_ context.Context, login globalauth.CompletedLogin) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pending[login.Challenge]; !ok {
		return time.Time{}, globalauth.ErrNotFound
	}
	if m.used[login.AssertionID] {
		return time.Time{}, globalauth.ErrReplay
	}
	if login.Grant.NodeID != thisNode || !globalauth.ValidGrant(login.Grant, time.Now()) {
		return time.Time{}, globalauth.ErrInvalidResponse
	}
	delete(m.pending, login.Challenge)
	m.used[login.AssertionID] = true
	m.grants[login.Grant.MemberID] = login.Grant
	if login.ReplacedDevice != nil {
		delete(m.devices, *login.ReplacedDevice)
	}
	m.devices[login.DeviceDigest] = login.Grant.MemberID
	expires := time.Now().Add(login.SessionTTL)
	if login.Grant.OfflineUntil.Before(expires) {
		expires = login.Grant.OfflineUntil
	}
	m.sessions[login.SessionDigest] = globalauth.Session{MemberID: login.Grant.MemberID, Permissions: []string{globalauth.PermissionMediaAccess}, ExpiresAt: expires}
	return expires, nil
}
func (m *memoryGlobalAuth) OfflineGrant(_ context.Context, device [32]byte) (globalauth.Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	member, ok := m.devices[device]
	grant, found := m.grants[member]
	if !ok || !found || !time.Now().Before(grant.OfflineUntil) {
		return globalauth.Grant{}, globalauth.ErrNotFound
	}
	return grant, nil
}
func (m *memoryGlobalAuth) CreateOfflineSession(ctx context.Context, device, session [32]byte, ttl time.Duration) (time.Time, error) {
	grant, err := m.OfflineGrant(ctx, device)
	if err != nil {
		return time.Time{}, globalauth.ErrInactive
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	expires := time.Now().Add(ttl)
	if grant.OfflineUntil.Before(expires) {
		expires = grant.OfflineUntil
	}
	m.sessions[session] = globalauth.Session{MemberID: grant.MemberID, Permissions: []string{globalauth.PermissionMediaAccess}, Offline: true, ExpiresAt: expires}
	return expires, nil
}
func (m *memoryGlobalAuth) RevokeAuthorization(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.grants, id)
	return nil
}
func (m *memoryGlobalAuth) CachedMemberIDs(context.Context, int) ([]string, error) { return nil, nil }
func (m *memoryGlobalAuth) ApplyRefresh(context.Context, string, []globalauth.RefreshState) error {
	return nil
}
func (m *memoryGlobalAuth) GetGlobalSession(_ context.Context, d [32]byte) (globalauth.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[d]
	if _, granted := m.grants[s.MemberID]; !ok || !granted || !time.Now().Before(s.ExpiresAt) {
		return globalauth.Session{}, globalauth.ErrNotFound
	}
	return s, nil
}
func (m *memoryGlobalAuth) DeleteGlobalSession(_ context.Context, d [32]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, d)
	return nil
}

type loginTest struct {
	repo    *memoryGlobalAuth
	handler http.Handler
	core    ed25519.PrivateKey
	// coreKeys is what Core publishes; coreErr simulates an outage.
	coreKeys []globalauth.VerificationKey
	coreErr  error
	fetches  int
}

func newLoginTest(t *testing.T) *loginTest {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lt := &loginTest{repo: newMemoryGlobalAuth(), core: key}
	lt.coreKeys = publish(key)
	identity := func(context.Context) (enrollment.Identity, error) {
		return enrollment.Identity{NodeID: thisNode, CoreURL: "https://core.internal", Status: "APPROVED", PrivateSeed: make([]byte, 32)}, nil
	}
	lt.handler = newRoutes(testRepository{}, globalAuthOpt{repository: lt.repo, identity: identity,
		login: &globalLoginConfig{coreURL: "https://core.internal", coreLoginURL: testCoreLogin, callbackURLs: []string{testCallbackURL}},
		fetchKeys: func(_ context.Context, coreURL string) ([]globalauth.VerificationKey, error) {
			lt.fetches++
			if coreURL != "https://core.internal" {
				return nil, errors.New("wrong Core URL")
			}
			return lt.coreKeys, lt.coreErr
		}})
	return lt
}

func publish(keys ...ed25519.PrivateKey) []globalauth.VerificationKey {
	var set []globalauth.VerificationKey
	for _, key := range keys {
		public := key.Public().(ed25519.PublicKey)
		set = append(set, globalauth.VerificationKey{ID: globalauth.KeyIDFor(public), PublicKey: public})
	}
	return set
}

type browser struct{ cookies map[string]*http.Cookie }

func newBrowser() *browser { return &browser{cookies: map[string]*http.Cookie{}} }

func (b *browser) call(t *testing.T, h http.Handler, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	for _, c := range b.cookies {
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	var decoded map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &decoded)
	return w, decoded
}

type started struct {
	nonce, challenge [32]byte
	redirect         string
}

func (lt *loginTest) start(t *testing.T, b *browser) started {
	t.Helper()
	w, body := b.call(t, lt.handler, "/api/v1/global-users/login", `{"callback_url":"`+testCallbackURL+`"}`)
	if w.Code != http.StatusOK || body["mode"] != "redirect" {
		t.Fatalf("start: %d %v", w.Code, body)
	}
	target, err := url.Parse(body["authorize_url"].(string))
	if err != nil || target.Scheme+"://"+target.Host != testCoreLogin || target.Path != "/node-login" {
		t.Fatalf("authorize URL %v", body["authorize_url"])
	}
	q := target.Query()
	var s started
	nonce, e1 := hex.DecodeString(q.Get("nonce"))
	challenge, e2 := hex.DecodeString(q.Get("challenge"))
	if e1 != nil || e2 != nil || len(nonce) != 32 || len(challenge) != 32 || q.Get("node_id") != thisNode || len(q) != 4 {
		t.Fatalf("authorize query %v", q)
	}
	copy(s.nonce[:], nonce)
	copy(s.challenge[:], challenge)
	s.redirect = q.Get("redirect_uri")
	return s
}

// assertion signs exactly what Core's signer would (core/internal/assertions).
func assertion(key ed25519.PrivateKey, node, member string, nonce, challenge [32]byte, issued, offline time.Time) string {
	payload := make([]byte, 180)
	payload[0], payload[1] = 1, 1
	id := globalauth.KeyIDFor(key.Public().(ed25519.PublicKey))
	copy(payload[2:], id[:])
	n, _ := hex.DecodeString(strings.TrimPrefix(node, "node_"))
	m, _ := hex.DecodeString(strings.TrimPrefix(member, "member_"))
	copy(payload[10:], n)
	copy(payload[42:], m)
	copy(payload[74:], nonce[:])
	copy(payload[106:], challenge[:])
	_, _ = rand.Read(payload[138:154])
	binary.BigEndian.PutUint64(payload[154:], uint64(issued.Unix()))
	binary.BigEndian.PutUint64(payload[162:], uint64(issued.Add(globalauth.AssertionLifetime).Unix()))
	payload[170], payload[171] = 1, 1
	binary.BigEndian.PutUint64(payload[172:], uint64(offline.Unix()))
	signature := ed25519.Sign(key, append([]byte(globalauth.AssertionDomain), payload...))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (lt *loginTest) sign(s started, member string) string {
	now := time.Now().UTC().Truncate(time.Second)
	return assertion(lt.core, thisNode, member, s.nonce, s.challenge, now, now.Add(30*24*time.Hour))
}

func complete(t *testing.T, lt *loginTest, b *browser, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return b.call(t, lt.handler, "/api/v1/global-users/login/complete", `{"assertion":"`+token+`"}`)
}

func TestPasswordLoginRouteIsGone(t *testing.T) {
	lt := newLoginTest(t)
	w, body := newBrowser().call(t, lt.handler, "/api/v1/global-users/sessions", `{"member_id":"`+aliceMember+`","password":"global correct horse battery"}`)
	if w.Code != http.StatusGone || body["error"] != "global_password_login_removed" || strings.Contains(w.Body.String(), "horse") {
		t.Fatalf("password route: %d %s", w.Code, w.Body.String())
	}
}

func TestOnlineLoginVerifiesAssertionAndCreatesSession(t *testing.T) {
	lt := newLoginTest(t)
	b := newBrowser()
	s := lt.start(t, b)
	state := b.cookies[globalLoginCookie]
	if state == nil || !state.HttpOnly || !state.Secure || state.SameSite != http.SameSiteLaxMode || state.Path != "/" || state.MaxAge != 300 {
		t.Fatalf("login state cookie %+v", state)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(state.Value)
	if sha256.Sum256(raw) != s.challenge || s.redirect != testCallbackURL {
		t.Fatal("challenge is not SHA-256 of the browser's login state")
	}
	w, body := complete(t, lt, b, lt.sign(s, aliceMember))
	if w.Code != http.StatusCreated || body["member_id"] != aliceMember || body["kind"] != "GLOBAL" || body["offline"] != false {
		t.Fatalf("complete: %d %v", w.Code, body)
	}
	if b.cookies[globalLoginCookie] != nil {
		t.Fatal("login state cookie not cleared")
	}
	device := b.cookies[globalDeviceCookie]
	if device == nil || !device.HttpOnly || !device.Secure || device.SameSite != http.SameSiteStrictMode || device.Path != "/" {
		t.Fatalf("device cookie %+v", device)
	}
	token := body["token"].(string)
	got, session := localCall(t, lt.handler, "GET", "/api/v1/global-users/session", "", "Authorization", "Bearer "+token)
	if got.Code != http.StatusOK || session["member_id"] != aliceMember {
		t.Fatalf("session: %d %v", got.Code, session)
	}
	if len(lt.repo.keys) != 1 {
		t.Fatal("Core's key set was not persisted at login start")
	}
}

func TestCompleteRejectsReplayForeignNodeAndUnboundAssertions(t *testing.T) {
	lt := newLoginTest(t)
	b := newBrowser()
	s := lt.start(t, b)
	token := lt.sign(s, aliceMember)
	if w, _ := complete(t, lt, b, token); w.Code != http.StatusCreated {
		t.Fatalf("first use: %d", w.Code)
	}
	// Replay: same browser state (already consumed) and a new login start.
	if w, body := complete(t, lt, b, token); w.Code != http.StatusUnauthorized || body["error"] != "invalid_global_login" {
		t.Fatalf("replay: %d %v", w.Code, body)
	}
	lt.start(t, b)
	if w, _ := complete(t, lt, b, token); w.Code != http.StatusUnauthorized {
		t.Fatalf("replay in a new login: %d", w.Code)
	}
	// An assertion for another Node, signed for this browser's login.
	other := newBrowser()
	s2 := lt.start(t, other)
	now := time.Now().UTC().Truncate(time.Second)
	if w, _ := complete(t, lt, other, assertion(lt.core, foreignNode, aliceMember, s2.nonce, s2.challenge, now, now.Add(time.Hour))); w.Code != http.StatusUnauthorized {
		t.Fatalf("foreign Node assertion: %d", w.Code)
	}
	// A captured assertion is useless to a browser that did not start it.
	victim, attacker := newBrowser(), newBrowser()
	sv := lt.start(t, victim)
	lt.start(t, attacker)
	if w, _ := complete(t, lt, attacker, lt.sign(sv, aliceMember)); w.Code != http.StatusUnauthorized {
		t.Fatalf("assertion accepted with another browser's login state: %d", w.Code)
	}
	if w, _ := complete(t, lt, newBrowser(), lt.sign(sv, aliceMember)); w.Code != http.StatusUnauthorized {
		t.Fatalf("assertion accepted without login state: %d", w.Code)
	}
	// A forged signature and an expired assertion.
	_, forger, _ := ed25519.GenerateKey(rand.Reader)
	forged := assertion(forger, thisNode, aliceMember, sv.nonce, sv.challenge, now, now.Add(time.Hour))
	if w, _ := complete(t, lt, victim, forged); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged assertion: %d", w.Code)
	}
	old := now.Add(-time.Hour)
	if w, _ := complete(t, lt, victim, assertion(lt.core, thisNode, aliceMember, sv.nonce, sv.challenge, old, now.Add(time.Hour))); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired assertion: %d", w.Code)
	}
	if w, _ := complete(t, lt, victim, lt.sign(sv, aliceMember)); w.Code != http.StatusCreated {
		t.Fatalf("failed attempts must not consume the victim's login: %d", w.Code)
	}
}

func TestOfflineLoginNeedsDeviceCredentialAndACoreOutage(t *testing.T) {
	lt := newLoginTest(t)
	b := newBrowser()
	if w, _ := complete(t, lt, b, lt.sign(lt.start(t, b), aliceMember)); w.Code != http.StatusCreated {
		t.Fatal("online login failed")
	}
	// Core reachable: a device credential never bypasses Core.
	if s := lt.start(t, b); s.redirect != testCallbackURL {
		t.Fatal("online start did not redirect")
	}
	lt.coreErr = globalauth.ErrUnavailable
	w, body := b.call(t, lt.handler, "/api/v1/global-users/login", `{}`)
	if w.Code != http.StatusCreated || body["mode"] != "session" || body["offline"] != true || body["member_id"] != aliceMember || body["token"] == "" {
		t.Fatalf("offline login: %d %v", w.Code, body)
	}
	if w, body := newBrowser().call(t, lt.handler, "/api/v1/global-users/login", `{}`); w.Code != http.StatusServiceUnavailable || body["error"] != "global_authentication_unavailable" {
		t.Fatalf("offline without device credential: %d %v", w.Code, body)
	}
	stranger := newBrowser()
	stranger.cookies[globalDeviceCookie] = &http.Cookie{Name: globalDeviceCookie, Value: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
	if w, body := stranger.call(t, lt.handler, "/api/v1/global-users/login", `{}`); w.Code != http.StatusUnauthorized || body["error"] != "offline_login_rejected" {
		t.Fatalf("offline with unknown device credential: %d %v", w.Code, body)
	}
	// After the offline window.
	lt.repo.mu.Lock()
	grant := lt.repo.grants[aliceMember]
	grant.OfflineUntil = time.Now().Add(-time.Second)
	lt.repo.grants[aliceMember] = grant
	lt.repo.mu.Unlock()
	if w, _ := b.call(t, lt.handler, "/api/v1/global-users/login", `{}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("offline login after the window: %d", w.Code)
	}
	// A malformed or untrusted Core answer is never an outage downgrade.
	lt.coreErr = globalauth.ErrInvalidResponse
	if w, _ := b.call(t, lt.handler, "/api/v1/global-users/login", `{}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid Core response: %d", w.Code)
	}
}

func TestOfflineLoginRechecksTheStoredGrantSignature(t *testing.T) {
	lt := newLoginTest(t)
	b := newBrowser()
	if w, _ := complete(t, lt, b, lt.sign(lt.start(t, b), aliceMember)); w.Code != http.StatusCreated {
		t.Fatal("online login failed")
	}
	lt.repo.mu.Lock()
	grant := lt.repo.grants[aliceMember]
	grant.Assertion = strings.Replace(grant.Assertion, ".", ".A", 1)[:len(grant.Assertion)]
	lt.repo.grants[aliceMember] = grant
	lt.repo.mu.Unlock()
	lt.coreErr = globalauth.ErrUnavailable
	if w, _ := b.call(t, lt.handler, "/api/v1/global-users/login", `{}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered stored grant: %d", w.Code)
	}
}

func TestRotatedKeyIsFetchedOnceWhenAnAssertionNamesIt(t *testing.T) {
	lt := newLoginTest(t)
	b := newBrowser()
	s := lt.start(t, b)
	_, rotated, _ := ed25519.GenerateKey(rand.Reader)
	lt.coreKeys = publish(rotated, lt.core)
	lt.core = rotated
	before := lt.fetches
	if w, body := complete(t, lt, b, lt.sign(s, aliceMember)); w.Code != http.StatusCreated || lt.fetches != before+1 {
		t.Fatalf("rotated key: %d %v fetches=%d", w.Code, body, lt.fetches-before)
	}
	// A key Core does not publish is still refused.
	s = lt.start(t, b)
	_, unknown, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC().Truncate(time.Second)
	if w, _ := complete(t, lt, b, assertion(unknown, thisNode, aliceMember, s.nonce, s.challenge, now, now.Add(time.Hour))); w.Code != http.StatusUnauthorized {
		t.Fatalf("unpublished key: %d", w.Code)
	}
}

func TestLoginStartRequiresARegisteredCallback(t *testing.T) {
	lt := newLoginTest(t)
	if w, body := newBrowser().call(t, lt.handler, "/api/v1/global-users/login", `{"callback_url":"https://evil.example/login/callback"}`); w.Code != http.StatusBadRequest || body["error"] != "invalid_callback_url" {
		t.Fatalf("unregistered callback: %d %v", w.Code, body)
	}
	disabled := newRoutes(testRepository{}, globalAuthOpt{repository: newMemoryGlobalAuth()})
	if w, body := newBrowser().call(t, disabled, "/api/v1/global-users/login", `{}`); w.Code != http.StatusServiceUnavailable || body["error"] != "global_login_not_configured" {
		t.Fatalf("login without configuration: %d %v", w.Code, body)
	}
}
