package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/core/internal/assertions"
	"arknet/core/internal/memberships"
	"arknet/core/internal/users"
)

const testCallback = "https://node-a.example/login/callback"

var otherNodeID = "node_" + strings.Repeat("b", 64)

// memoryLogin follows the assertions.Repository contract on top of the
// in-memory users, memberships and signed Node identities.
type memoryLogin struct {
	*memoryInvitations
	lmu       sync.Mutex
	callbacks map[string][]string
	nonces    map[string]time.Time
}

func newMemoryLogin(t *testing.T) (*memoryLogin, ed25519.PrivateKey, ed25519.PrivateKey) {
	t.Helper()
	repo, key := newMemoryInvitations(t)
	otherPublic, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	repo.otherNodes = map[string][]byte{otherNodeID: otherPublic}
	return &memoryLogin{memoryInvitations: repo, callbacks: map[string][]string{}, nonces: map[string]time.Time{}}, key, otherKey
}

func (m *memoryLogin) approved(nodeID string) bool {
	node, _, err := m.EnrollmentIdentity(context.Background(), nodeID)
	return err == nil && node.EnrollmentStatus == "APPROVED" && node.TrustStatus == "TRUSTED"
}

func (m *memoryLogin) LoginCallbacks(_ context.Context, nodeID string) ([]string, error) {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	if !m.approved(nodeID) {
		return nil, assertions.ErrDenied
	}
	return append([]string(nil), m.callbacks[nodeID]...), nil
}

func (m *memoryLogin) SetLoginCallbacks(_ context.Context, nodeID string, urls []string) error {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	if !m.approved(nodeID) {
		return assertions.ErrDenied
	}
	m.callbacks[nodeID] = append([]string(nil), urls...)
	return nil
}

func (m *memoryLogin) LoginMembership(_ context.Context, nodeID, userID string) (assertions.Membership, error) {
	if !m.approved(nodeID) {
		return assertions.Membership{}, assertions.ErrDenied
	}
	m.imu.Lock()
	member, ok := m.members[nodeID+"|"+userID]
	m.imu.Unlock()
	m.memoryUsers.mu.Lock()
	user := m.byID[userID]
	m.memoryUsers.mu.Unlock()
	if !ok || user == nil || user.user.Status != users.StatusActive {
		return assertions.Membership{}, assertions.ErrDenied
	}
	return assertions.Membership{MemberID: member.MemberID, Role: member.Role}, nil
}

func (m *memoryLogin) ClaimLoginNonce(_ context.Context, nodeID string, digest [32]byte, retention time.Duration) error {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	key := nodeID + "|" + hex.EncodeToString(digest[:])
	if _, used := m.nonces[key]; used {
		return assertions.ErrNonceUsed
	}
	m.nonces[key] = time.Now().Add(retention)
	return nil
}

type loginFixture struct {
	repo     *memoryLogin
	nodeKey  ed25519.PrivateKey
	otherKey ed25519.PrivateKey
	signer   *assertions.Signer
	handler  http.Handler
	logs     *syncBuffer
	now      time.Time
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

func newLoginFixture(t *testing.T, published ...ed25519.PublicKey) *loginFixture {
	t.Helper()
	repo, nodeKey, otherKey := newMemoryLogin(t)
	_, signingKey, _ := ed25519.GenerateKey(rand.Reader)
	return newLoginFixtureWith(t, repo, nodeKey, otherKey, signingKey, published...)
}

func newLoginFixtureWith(t *testing.T, repo *memoryLogin, nodeKey, otherKey, signingKey ed25519.PrivateKey, published ...ed25519.PublicKey) *loginFixture {
	t.Helper()
	signer, err := assertions.NewSigner(signingKey, assertions.MaxOfflineWindow)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	login := &nodeLoginConfig{signer: signer, published: published, logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	return &loginFixture{repo: repo, nodeKey: nodeKey, otherKey: otherKey, signer: signer, logs: logs, now: time.Now(),
		handler: withAdministration(newCoreHandler(healthyDatabase{}, nil, nil, login, repo), repo, allowAdmin(true))}
}

func (f *loginFixture) registerCallbacks(t *testing.T, nodeID string, key ed25519.PrivateKey, urls ...string) int {
	t.Helper()
	body, _ := json.Marshal(map[string][]string{"callback_urls": urls})
	request := signedRequest("PUT", "/api/federation/v1/nodes/"+nodeID+"/login-callbacks", string(body), nodeID, key, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response.Code
}

// member creates an active user who belongs to nodeID and returns its pairwise ID.
func (f *loginFixture) member(t *testing.T, nodeID, username string) string {
	t.Helper()
	createUser(t, f.handler, username)
	f.repo.memoryUsers.mu.Lock()
	userID := f.repo.byName[username].user.ID
	f.repo.memoryUsers.mu.Unlock()
	memberID, _ := memberships.NewMemberID()
	f.repo.imu.Lock()
	f.repo.members[nodeID+"|"+userID] = memberships.Member{MemberID: memberID, Role: memberships.RoleMember, JoinedAt: time.Now()}
	f.repo.imu.Unlock()
	return memberID
}

type loginStart struct {
	nodeID, callback string
	nonce, challenge [32]byte
}

func newStart(nodeID, callback string) loginStart {
	s := loginStart{nodeID: nodeID, callback: callback}
	_, _ = rand.Read(s.nonce[:])
	verifier := make([]byte, 32)
	_, _ = rand.Read(verifier)
	s.challenge = sha256.Sum256(verifier)
	return s
}

func (s loginStart) values() url.Values {
	return url.Values{"node_id": {s.nodeID}, "nonce": {hex.EncodeToString(s.nonce[:])},
		"challenge": {hex.EncodeToString(s.challenge[:])}, "redirect_uri": {s.callback}}
}

func (f *loginFixture) page(s loginStart) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, httptest.NewRequest("GET", "/node-login?"+s.values().Encode(), nil))
	return response
}

func (f *loginFixture) submit(s loginStart, username, password string, headers ...string) *httptest.ResponseRecorder {
	form := s.values()
	form.Set("username", username)
	form.Set("password", password)
	request := httptest.NewRequest("POST", "/node-login", strings.NewReader(form.Encode()))
	request.RemoteAddr = nextTestPeer()
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func (f *loginFixture) keys() map[assertions.KeyID]ed25519.PublicKey {
	return map[assertions.KeyID]ed25519.PublicKey{f.signer.KeyID(): f.signer.PublicKey()}
}

// assertionFrom returns the assertion from a successful redirect.
func assertionFrom(t *testing.T, response *httptest.ResponseRecorder, callback string) string {
	t.Helper()
	if response.Code != http.StatusSeeOther {
		t.Fatalf("login: HTTP %d %s", response.Code, response.Body)
	}
	location := response.Header().Get("Location")
	prefix := callback + "#assertion="
	if !strings.HasPrefix(location, prefix) {
		t.Fatalf("redirect %q is not the registered callback with a fragment", location)
	}
	return strings.TrimPrefix(location, prefix)
}

func (s loginStart) expectation() assertions.Expectation {
	return assertions.Expectation{NodeID: s.nodeID, Nonce: s.nonce, Challenge: s.challenge}
}

func TestNodeLoginIssuesAssertionBoundToNodeMemberNonceAndChallenge(t *testing.T) {
	f := newLoginFixture(t)
	if code := f.registerCallbacks(t, enrollmentID, f.nodeKey, testCallback); code != http.StatusNoContent {
		t.Fatalf("register callbacks: %d", code)
	}
	memberID := f.member(t, enrollmentID, "alice")
	start := newStart(enrollmentID, testCallback)
	page := f.page(start)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="password"`) ||
		!strings.Contains(page.Body.String(), "https://node-a.example") {
		t.Fatalf("login page: %d %s", page.Code, page.Body)
	}
	csp := page.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"frame-ancestors 'none'", "form-action 'self' https://node-a.example", "default-src 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP %q lacks %q", csp, directive)
		}
	}
	if page.Header().Get("X-Frame-Options") != "DENY" || page.Header().Get("Cache-Control") != "no-store" ||
		page.Header().Get("Referrer-Policy") != "same-origin" {
		t.Errorf("page headers: %v", page.Header())
	}
	response := f.submit(start, "alice", testPassword)
	token := assertionFrom(t, response, testCallback)
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("redirect headers: %v", response.Header())
	}
	claims, err := assertions.Verify(token, f.keys(), start.expectation(), time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.NodeID != enrollmentID || claims.MemberID != memberID || claims.Role != memberships.RoleMember {
		t.Fatalf("binding: %+v", claims)
	}
	if strings.Contains(token, memberID) || strings.Contains(response.Header().Get("Location"), "user_") {
		t.Fatal("assertion must carry raw pairwise bytes only, never a global user ID")
	}
}

func TestNodeLoginWrongMemberGetsOwnPairwiseIDOrIsDenied(t *testing.T) {
	f := newLoginFixture(t)
	f.registerCallbacks(t, enrollmentID, f.nodeKey, testCallback)
	aliceID := f.member(t, enrollmentID, "alice")
	bobID := f.member(t, enrollmentID, "bob")
	start := newStart(enrollmentID, testCallback)
	claims, err := assertions.Verify(assertionFrom(t, f.submit(start, "bob", testPassword), testCallback), f.keys(), start.expectation(), time.Now())
	if err != nil || claims.MemberID != bobID || claims.MemberID == aliceID {
		t.Fatalf("bob's assertion must name bob's membership: %+v %v", claims, err)
	}
	// A user with no membership on this Node gets no assertion, and the
	// failed attempt does not consume the nonce.
	createUser(t, f.handler, "carol")
	start = newStart(enrollmentID, testCallback)
	denied := f.submit(start, "carol", testPassword)
	if denied.Code != http.StatusForbidden || denied.Header().Get("Location") != "" {
		t.Fatalf("non-member: %d %v", denied.Code, denied.Header())
	}
	assertionFrom(t, f.submit(start, "alice", testPassword), testCallback)
}

func TestNodeLoginWrongNodeIsRefused(t *testing.T) {
	f := newLoginFixture(t)
	f.registerCallbacks(t, enrollmentID, f.nodeKey, testCallback)
	f.registerCallbacks(t, otherNodeID, f.otherKey, "https://node-b.example/login/callback")
	f.member(t, enrollmentID, "alice")
	// Node A's callback cannot be used with Node B's ID (and vice versa).
	crossed := newStart(otherNodeID, testCallback)
	if page := f.page(crossed); page.Code != http.StatusBadRequest || strings.Contains(page.Body.String(), `name="password"`) {
		t.Fatalf("crossed callback page: %d", page.Code)
	}
	if response := f.submit(crossed, "alice", testPassword); response.Code != http.StatusBadRequest || response.Header().Get("Location") != "" {
		t.Fatalf("crossed callback login: %d %v", response.Code, response.Header())
	}
	// Alice is not a member of Node B, so even its own callback yields nothing.
	other := newStart(otherNodeID, "https://node-b.example/login/callback")
	if response := f.submit(other, "alice", testPassword); response.Code != http.StatusForbidden {
		t.Fatalf("non-member of Node B: %d", response.Code)
	}
	// An assertion for Node A never verifies at Node B.
	start := newStart(enrollmentID, testCallback)
	token := assertionFrom(t, f.submit(start, "alice", testPassword), testCallback)
	atB := start.expectation()
	atB.NodeID = otherNodeID
	if _, err := assertions.Verify(token, f.keys(), atB, time.Now()); err == nil {
		t.Fatal("Node A assertion verified as Node B")
	}
	// Unknown Nodes and unregistered callbacks share one refusal and never redirect.
	for _, start := range []loginStart{newStart("node_"+strings.Repeat("c", 64), testCallback), newStart(enrollmentID, "https://evil.example/login/callback")} {
		page := f.page(start)
		if page.Code != http.StatusBadRequest || page.Header().Get("Location") != "" || !strings.Contains(page.Body.String(), "not valid") {
			t.Errorf("%s: %d", start.callback, page.Code)
		}
	}
}

func TestNodeLoginNonceIsSingleUse(t *testing.T) {
	f := newLoginFixture(t)
	f.registerCallbacks(t, enrollmentID, f.nodeKey, testCallback)
	f.member(t, enrollmentID, "alice")
	f.member(t, enrollmentID, "bob")
	start := newStart(enrollmentID, testCallback)
	assertionFrom(t, f.submit(start, "alice", testPassword), testCallback)
	for _, user := range []string{"alice", "bob"} {
		replay := f.submit(start, user, testPassword)
		if replay.Code != http.StatusConflict || replay.Header().Get("Location") != "" {
			t.Fatalf("%s reusing a nonce: %d %v", user, replay.Code, replay.Header())
		}
	}
	// The same nonce on another Node is a different claim.
	f.registerCallbacks(t, otherNodeID, f.otherKey, "https://node-b.example/login/callback")
	f.member(t, otherNodeID, "carol")
	other := start
	other.nodeID, other.callback = otherNodeID, "https://node-b.example/login/callback"
	assertionFrom(t, f.submit(other, "carol", testPassword), other.callback)
}

func TestNodeLoginAssertionExpires(t *testing.T) {
	f := newLoginFixture(t)
	f.registerCallbacks(t, enrollmentID, f.nodeKey, testCallback)
	f.member(t, enrollmentID, "alice")
	start := newStart(enrollmentID, testCallback)
	claims, err := assertions.Verify(assertionFrom(t, f.submit(start, "alice", testPassword), testCallback), f.keys(), start.expectation(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := claims.ExpiresAt.Sub(claims.IssuedAt); got != assertions.Lifetime {
		t.Fatalf("lifetime %s", got)
	}
	if got := claims.OfflineUntil.Sub(claims.IssuedAt); got != assertions.MaxOfflineWindow {
		t.Fatalf("offline window %s", got)
	}
	start2 := newStart(enrollmentID, testCallback)
	token := assertionFrom(t, f.submit(start2, "alice", testPassword), testCallback)
	if _, err := assertions.Verify(token, f.keys(), start2.expectation(), claims.ExpiresAt.Add(assertions.ClockSkew+time.Second)); err == nil {
		t.Fatal("expired assertion verified")
	}
}

func TestNodeLoginRejectsBadCredentialsDisabledUsersAndCrossSitePosts(t *testing.T) {
	f := newLoginFixture(t)
	f.registerCallbacks(t, enrollmentID, f.nodeKey, testCallback)
	f.member(t, enrollmentID, "alice")
	f.member(t, enrollmentID, "dora")
	start := newStart(enrollmentID, testCallback)
	for name, response := range map[string]*httptest.ResponseRecorder{
		"wrong password": f.submit(start, "alice", "wrong password value"),
		"unknown user":   f.submit(start, "nobody", testPassword),
		"short password": f.submit(start, "alice", "short"),
	} {
		if response.Code != http.StatusUnauthorized || response.Header().Get("Location") != "" ||
			!strings.Contains(response.Body.String(), "Invalid username or password") {
			t.Errorf("%s: %d %v", name, response.Code, response.Header())
		}
	}
	f.repo.memoryUsers.mu.Lock()
	f.repo.byName["dora"].user.Status = users.StatusDisabled
	f.repo.memoryUsers.mu.Unlock()
	if response := f.submit(start, "dora", testPassword); response.Code != http.StatusUnauthorized {
		t.Errorf("disabled user: %d", response.Code)
	}
	if response := f.submit(start, "alice", testPassword, "Sec-Fetch-Site", "cross-site"); response.Code != http.StatusForbidden || response.Header().Get("Location") != "" {
		t.Errorf("cross-site post: %d", response.Code)
	}
	// None of the failures consumed the nonce.
	assertionFrom(t, f.submit(start, "alice", testPassword), testCallback)
	// Extra or duplicated form fields are refused.
	form := start.values()
	form.Add("redirect_uri", "https://evil.example/cb")
	form.Set("username", "alice")
	form.Set("password", testPassword)
	request := httptest.NewRequest("POST", "/node-login", strings.NewReader(form.Encode()))
	request.RemoteAddr = nextTestPeer()
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || response.Header().Get("Location") != "" {
		t.Errorf("duplicated redirect_uri: %d %v", response.Code, response.Header())
	}
}

func TestLoginCallbackRegistrationRequiresSignedApprovedNodeAndValidURLs(t *testing.T) {
	f := newLoginFixture(t)
	if code := f.registerCallbacks(t, enrollmentID, f.otherKey, testCallback); code != http.StatusUnauthorized {
		t.Fatalf("wrong Node key: %d", code)
	}
	for _, urls := range [][]string{{}, {"http://node-a.example/cb"}, {testCallback, testCallback},
		{"https://a.example/1", "https://a.example/2", "https://a.example/3", "https://a.example/4", "https://a.example/5"}} {
		if code := f.registerCallbacks(t, enrollmentID, f.nodeKey, urls...); code != http.StatusBadRequest {
			t.Errorf("%v: %d", urls, code)
		}
	}
	if code := f.registerCallbacks(t, enrollmentID, f.nodeKey, testCallback, "http://127.0.0.1:5173/login/callback"); code != http.StatusNoContent {
		t.Fatalf("valid callbacks: %d", code)
	}
	// Replacing the set removes the old callback.
	f.registerCallbacks(t, enrollmentID, f.nodeKey, "https://node-a.example/new/callback")
	if page := f.page(newStart(enrollmentID, testCallback)); page.Code != http.StatusBadRequest {
		t.Fatalf("replaced callback still accepted: %d", page.Code)
	}
	f.repo.node.TrustStatus = "BLOCKED"
	if code := f.registerCallbacks(t, enrollmentID, f.nodeKey, testCallback); code != http.StatusForbidden {
		t.Fatalf("blocked Node: %d", code)
	}
}

func TestAssertionKeySetPublishesActiveAndRotatedKeys(t *testing.T) {
	repo, nodeKey, otherKey := newMemoryLogin(t)
	_, oldKey, _ := ed25519.GenerateKey(rand.Reader)
	_, newKey, _ := ed25519.GenerateKey(rand.Reader)
	before := newLoginFixtureWith(t, repo, nodeKey, otherKey, oldKey)
	before.registerCallbacks(t, enrollmentID, nodeKey, testCallback)
	before.member(t, enrollmentID, "alice")
	start := newStart(enrollmentID, testCallback)
	oldToken := assertionFrom(t, before.submit(start, "alice", testPassword), testCallback)

	// Rotate: the new key signs, the old public key stays published.
	after := newLoginFixtureWith(t, repo, nodeKey, otherKey, newKey, oldKey.Public().(ed25519.PublicKey))
	response := httptest.NewRecorder()
	after.handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/federation/v1/assertion-keys", nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "public, max-age=3600" {
		t.Fatalf("key set: %d %v", response.Code, response.Header())
	}
	var set struct {
		Keys []struct {
			KeyID     string `json:"key_id"`
			Algorithm string `json:"algorithm"`
			PublicKey string `json:"public_key"`
			Status    string `json:"status"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &set); err != nil || len(set.Keys) != 2 {
		t.Fatalf("key set body: %s", response.Body)
	}
	published := map[assertions.KeyID]ed25519.PublicKey{}
	statuses := map[string]string{}
	for _, key := range set.Keys {
		public, err := base64.RawURLEncoding.DecodeString(key.PublicKey)
		if err != nil || len(public) != ed25519.PublicKeySize || key.Algorithm != "Ed25519" {
			t.Fatalf("published key: %+v", key)
		}
		id := assertions.KeyIDFor(public)
		if id.String() != key.KeyID {
			t.Fatalf("key ID %s does not match its key", key.KeyID)
		}
		published[id] = public
		statuses[key.KeyID] = key.Status
	}
	if statuses[after.signer.KeyID().String()] != "active" || statuses[assertions.KeyIDFor(oldKey.Public().(ed25519.PublicKey)).String()] != "published" {
		t.Fatalf("statuses: %v", statuses)
	}
	start2 := newStart(enrollmentID, testCallback)
	newClaims, err := assertions.Verify(assertionFrom(t, after.submit(start2, "alice", testPassword), testCallback), published, start2.expectation(), time.Now())
	if err != nil || newClaims.KeyID != after.signer.KeyID() {
		t.Fatalf("new assertion: %+v %v", newClaims, err)
	}
	if _, err := assertions.Verify(oldToken, published, start.expectation(), time.Now()); err != nil {
		t.Fatalf("assertion from the rotated-out key: %v", err)
	}
	if strings.Contains(response.Body.String(), base64.StdEncoding.EncodeToString(newKey.Seed())) ||
		strings.Contains(response.Body.String(), base64.RawURLEncoding.EncodeToString(newKey.Seed())) {
		t.Fatal("private key material published")
	}
}

func TestNodeLoginLogsNeitherPasswordsNorAssertions(t *testing.T) {
	f := newLoginFixture(t)
	f.registerCallbacks(t, enrollmentID, f.nodeKey, testCallback)
	memberID := f.member(t, enrollmentID, "alice")
	createUser(t, f.handler, "carol")
	// Real HTTP server so net/http's own error log is captured too.
	server := httptest.NewUnstartedServer(f.handler)
	server.Config.ErrorLog = log.New(f.logs, "", 0)
	server.Start()
	defer server.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(s loginStart, username, password string) *http.Response {
		form := s.values()
		form.Set("username", username)
		form.Set("password", password)
		request, _ := http.NewRequest("POST", server.URL+"/node-login", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response
	}
	start := newStart(enrollmentID, testCallback)
	wrongPassword := "wrong-" + hex.EncodeToString(start.challenge[:8])
	post(start, "alice", wrongPassword)
	post(start, "carol", testPassword)
	success := post(start, "alice", testPassword)
	replay := post(start, "alice", testPassword)
	token := strings.TrimPrefix(success.Header.Get("Location"), testCallback+"#assertion=")
	if success.StatusCode != http.StatusSeeOther || token == "" || replay.StatusCode != http.StatusConflict {
		t.Fatalf("flow: %d %d", success.StatusCode, replay.StatusCode)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "node_login_assertion_issued") || !strings.Contains(logs, "node_login_denied") {
		t.Fatalf("expected login events were not logged: %s", logs)
	}
	payload, signature, _ := strings.Cut(token, ".")
	for name, secret := range map[string]string{
		"password": testPassword, "wrong password": wrongPassword, "assertion": token,
		"assertion payload": payload, "assertion signature": signature,
		"nonce": hex.EncodeToString(start.nonce[:]), "challenge": hex.EncodeToString(start.challenge[:]),
		"member ID": memberID, "username": "alice", "carol": "carol", "callback": testCallback,
		"user ID": f.repo.byName["alice"].user.ID,
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("Core log contains the %s", name)
		}
	}
}

func TestNodeLoginRefusesMalformedSubmissionsAndUntrustedNodes(t *testing.T) {
	f := newLoginFixture(t)
	f.registerCallbacks(t, enrollmentID, f.nodeKey, testCallback)
	f.member(t, enrollmentID, "alice")
	start := newStart(enrollmentID, testCallback)
	form := start.values()
	form.Set("username", "alice")
	form.Set("password", testPassword)
	send := func(target, contentType, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", target, strings.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		return response
	}
	for name, response := range map[string]*httptest.ResponseRecorder{
		"query string":  send("/node-login?x=1", "application/x-www-form-urlencoded", form.Encode()),
		"json body":     send("/node-login", "application/json", `{"username":"alice"}`),
		"oversize body": send("/node-login", "application/x-www-form-urlencoded", form.Encode()+"&pad="+strings.Repeat("a", 5000)),
	} {
		if response.Code != http.StatusBadRequest || response.Header().Get("Location") != "" {
			t.Errorf("%s: %d %v", name, response.Code, response.Header())
		}
	}
	// A re-rendered page after a failed login keeps the same protections.
	failed := f.submit(start, "alice", "wrong password value")
	if !strings.Contains(failed.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		failed.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("401 page headers: %v", failed.Header())
	}
	// An ADMIN membership is carried through to the assertion.
	f.repo.imu.Lock()
	for key, member := range f.repo.members {
		member.Role = memberships.RoleAdmin
		f.repo.members[key] = member
	}
	f.repo.imu.Unlock()
	admin := newStart(enrollmentID, testCallback)
	claims, err := assertions.Verify(assertionFrom(t, f.submit(admin, "alice", testPassword), testCallback), f.keys(), admin.expectation(), time.Now())
	if err != nil || claims.Role != memberships.RoleAdmin {
		t.Fatalf("admin role: %+v %v", claims, err)
	}
	// Once the Node is blocked, neither the page nor the submission works.
	f.repo.node.TrustStatus = "BLOCKED"
	blocked := newStart(enrollmentID, testCallback)
	if page := f.page(blocked); page.Code != http.StatusBadRequest || strings.Contains(page.Body.String(), `name="password"`) {
		t.Errorf("blocked Node page: %d", page.Code)
	}
	if response := f.submit(blocked, "alice", testPassword); response.Code != http.StatusBadRequest || response.Header().Get("Location") != "" {
		t.Errorf("blocked Node login: %d %v", response.Code, response.Header())
	}
}
