package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
	"arknet/core/internal/users"
)

type memoryInvitation struct {
	nodeID    string
	role      memberships.Role
	maxUses   int
	uses      int
	expiresAt time.Time
}

// memoryInvitations follows the memberships.Repository contract on top of the
// in-memory users store and a single signed Node identity.
type memoryInvitations struct {
	*memoryUsers
	node        nodes.Node
	key         []byte
	otherNodes  map[string][]byte // additional approved Node identities
	imu         sync.Mutex
	invitations map[[32]byte]*memoryInvitation
	proofs      map[[32]byte]bool
	members     map[string]memberships.Member // node|user
}

func newMemoryInvitations(t *testing.T) (*memoryInvitations, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &memoryInvitations{
		memoryUsers: newMemoryUsers(),
		node:        nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted},
		key:         pub, invitations: map[[32]byte]*memoryInvitation{}, proofs: map[[32]byte]bool{}, members: map[string]memberships.Member{},
	}, priv
}

func (m *memoryInvitations) RegisterEnrollment(context.Context, string, string, []byte) (nodes.Node, error) {
	return m.node, nil
}
func (m *memoryInvitations) EnrollmentIdentity(_ context.Context, id string) (nodes.Node, []byte, error) {
	if key, ok := m.otherNodes[id]; ok {
		return nodes.Node{PublicID: id, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted}, key, nil
	}
	if id != m.node.PublicID {
		return nodes.Node{}, nil, nodes.ErrNotFound
	}
	return m.node, m.key, nil
}

func (m *memoryInvitations) CreateInvitation(_ context.Context, nodeID string, role memberships.Role, expiresAt time.Time, maxUses int, digest, proof [32]byte) (memberships.Invitation, error) {
	m.imu.Lock()
	defer m.imu.Unlock()
	if m.proofs[proof] {
		return memberships.Invitation{}, memberships.ErrReplayedCreate
	}
	m.proofs[proof] = true
	m.invitations[digest] = &memoryInvitation{nodeID, role, maxUses, 0, expiresAt}
	id, _ := memberships.NewInvitationID()
	return memberships.Invitation{ID: id, NodeID: nodeID, Role: role, MaxUses: maxUses, ExpiresAt: expiresAt, CreatedAt: time.Now()}, nil
}

func (m *memoryInvitations) AcceptInvitation(_ context.Context, digest [32]byte, userID string) (memberships.Membership, error) {
	m.imu.Lock()
	defer m.imu.Unlock()
	invitation := m.invitations[digest]
	if invitation == nil || !time.Now().Before(invitation.expiresAt) || invitation.uses >= invitation.maxUses {
		return memberships.Membership{}, memberships.ErrUnavailable
	}
	m.memoryUsers.mu.Lock()
	user := m.byID[userID]
	m.memoryUsers.mu.Unlock()
	if user == nil || user.user.Status != users.StatusActive {
		return memberships.Membership{}, users.ErrInactive
	}
	key := invitation.nodeID + "|" + userID
	if _, exists := m.members[key]; exists {
		return memberships.Membership{}, memberships.ErrAlreadyMember
	}
	memberID, _ := memberships.NewMemberID()
	member := memberships.Member{MemberID: memberID, Role: invitation.role, JoinedAt: time.Now()}
	m.members[key] = member
	invitation.uses++
	return memberships.Membership{NodeID: invitation.nodeID, MemberID: memberID, Role: invitation.role, CreatedAt: member.JoinedAt}, nil
}

func invitationServer(repo *memoryInvitations) http.Handler {
	return withAdministration(newHandler(healthyDatabase{}, repo), repo, allowAdmin(true))
}

const invitationsPath = "/api/federation/v1/nodes/%s/invitations"

func createInvitation(t *testing.T, h http.Handler, key ed25519.PrivateKey, body string) (*httptest.ResponseRecorder, map[string]any, *http.Request) {
	t.Helper()
	request := signedRequest("POST", fmt.Sprintf(invitationsPath, enrollmentID), body, enrollmentID, key, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	var decoded map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response, decoded, request
}

func policy(role string, expires time.Duration, uses int) string {
	return fmt.Sprintf(`{"role":%q,"expires_at":%q,"max_uses":%d}`, role, time.Now().Add(expires).UTC().Format(time.RFC3339), uses)
}

func sessionFor(t *testing.T, h http.Handler, username string) string {
	t.Helper()
	createUser(t, h, username)
	_, body := login(t, h, username, testPassword)
	return body["token"].(string)
}

func accept(t *testing.T, h http.Handler, token, secret string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	encoded, _ := json.Marshal(map[string]string{"secret": secret})
	if token == "" {
		return call(t, h, "POST", "/api/identity/v1/invitations/accept", string(encoded))
	}
	return call(t, h, "POST", "/api/identity/v1/invitations/accept", string(encoded), "Authorization", "Bearer "+token)
}

func TestSignedNodeCreatesInvitationWithoutUserData(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	h := invitationServer(repo)
	sessionFor(t, h, "unrelated")
	response, body, _ := createInvitation(t, h, key, policy("ADMIN", time.Hour, 3))
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	var keys []string
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "expires_at,invitation_id,max_uses,node_id,role,secret" || body["node_id"] != enrollmentID || body["role"] != "ADMIN" || body["max_uses"] != float64(3) {
		t.Fatalf("response %s", response.Body.String())
	}
	secret := body["secret"].(string)
	digest, ok := memberships.SecretDigest(secret)
	if !ok || repo.invitations[digest] == nil {
		t.Fatal("stored invitation is not keyed by the secret digest")
	}
	if strings.Contains(response.Body.String(), "user_") || strings.Contains(response.Body.String(), "unrelated") {
		t.Fatalf("invitation response exposes global users: %s", response.Body.String())
	}
}

func TestInvitationCreationRejectsUserTargetingAndInvalidPolicy(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	h := invitationServer(repo)
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, body := range []string{
		fmt.Sprintf(`{"role":"MEMBER","expires_at":%q,"max_uses":1,"username":"alice"}`, expires),
		fmt.Sprintf(`{"role":"MEMBER","expires_at":%q,"max_uses":1,"user_id":"user_%s"}`, expires, strings.Repeat("0", 64)),
		fmt.Sprintf(`{"role":"MEMBER","expires_at":%q}`, expires),
		fmt.Sprintf(`{"role":"MEMBER","expires_at":%q,"max_uses":1.5}`, expires),
		fmt.Sprintf(`{"role":"MEMBER","expires_at":%q,"max_uses":"1"}`, expires),
		`{"role":"MEMBER","expires_at":"tomorrow","max_uses":1}`,
		policy("OWNER", time.Hour, 1),
		policy("MEMBER", -time.Minute, 1),
		policy("MEMBER", 31*24*time.Hour, 1),
		policy("MEMBER", time.Hour, 0),
		policy("MEMBER", time.Hour, 101),
	} {
		if response, _, _ := createInvitation(t, h, key, body); response.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, response.Code, response.Body.String())
		}
	}
	if len(repo.invitations) != 0 {
		t.Fatal("invalid request created an invitation")
	}
}

func TestInvitationCreationRequiresOwnApprovedNodeSignatureAndRejectsReplay(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	h := invitationServer(repo)
	body := policy("MEMBER", time.Hour, 1)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	ts := fmt.Sprint(time.Now().Unix())
	other := "node_" + strings.Repeat("b", 64)
	for name, request := range map[string]*http.Request{
		"unsigned":        httptest.NewRequest("POST", fmt.Sprintf(invitationsPath, enrollmentID), strings.NewReader(body)),
		"wrong key":       signedRequest("POST", fmt.Sprintf(invitationsPath, enrollmentID), body, enrollmentID, otherKey, ts),
		"other node path": signedRequest("POST", fmt.Sprintf(invitationsPath, other), body, enrollmentID, key, ts),
		"stale":           signedRequest("POST", fmt.Sprintf(invitationsPath, enrollmentID), body, enrollmentID, key, fmt.Sprint(time.Now().Add(-5*time.Minute).Unix())),
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d", name, response.Code)
		}
	}
	response, _, request := createInvitation(t, h, key, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d", response.Code)
	}
	replay := httptest.NewRequest("POST", request.URL.Path, strings.NewReader(body))
	replay.Header = request.Header.Clone()
	replayed := httptest.NewRecorder()
	h.ServeHTTP(replayed, replay)
	if replayed.Code != http.StatusConflict || len(repo.invitations) != 1 {
		t.Fatalf("replay: %d invitations=%d", replayed.Code, len(repo.invitations))
	}
	repo.node.TrustStatus = nodes.TrustBlocked
	if response, _, _ := createInvitation(t, h, key, policy("MEMBER", time.Hour, 2)); response.Code != http.StatusForbidden {
		t.Fatalf("blocked node: %d", response.Code)
	}
}

func TestNodeSignatureCannotBrowseGlobalUsers(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	sessionFor(t, invitationServer(repo), "alice")
	// Production shape: a Node signature carries no administrative authority.
	h := withAdministration(newHandler(healthyDatabase{}, repo), repo, allowAdmin(false))
	ts := fmt.Sprint(time.Now().Unix())
	for _, target := range []struct{ method, path string }{
		{"GET", "/api/admin/v1/users"},
		{"POST", "/api/admin/v1/users"},
		{"GET", "/api/identity/v1/session"},
		{"GET", fmt.Sprintf(invitationsPath, enrollmentID)},
		{"GET", "/api/federation/v1/nodes/" + enrollmentID + "/members"},
		{"GET", "/api/federation/v1/users"},
		{"GET", "/api/federation/v1/nodes/" + enrollmentID + "/memberships"},
	} {
		request := signedRequest(target.method, target.path, "", enrollmentID, key, ts)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code < 400 || strings.Contains(response.Body.String(), "alice") || strings.Contains(response.Body.String(), "user_") {
			t.Fatalf("%s %s: %d %s", target.method, target.path, response.Code, response.Body.String())
		}
	}
}

func TestMembershipEnumerationRouteDoesNotExist(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	h := invitationServer(repo)
	response, _ := signedJSON(t, h, "GET", "/api/federation/v1/nodes/"+enrollmentID+"/memberships", "", enrollmentID, key)
	if response.Code != http.StatusNotFound {
		t.Fatalf("membership enumeration route: %d %s", response.Code, response.Body)
	}
}

func TestInvitationAcceptanceFollowsPolicy(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	h := invitationServer(repo)
	alice, bob, carol := sessionFor(t, h, "alice"), sessionFor(t, h, "bob"), sessionFor(t, h, "carol")

	_, created, _ := createInvitation(t, h, key, policy("MEMBER", time.Hour, 1))
	secret := created["secret"].(string)
	if response, _ := accept(t, h, "", secret); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated acceptance: %d", response.Code)
	}
	response, body := accept(t, h, alice, secret)
	if response.Code != http.StatusCreated || body["node_id"] != enrollmentID || body["role"] != "MEMBER" || body["joined_at"] == nil || len(body) != 4 || !memberships.ValidMemberID(body["member_id"].(string)) {
		t.Fatalf("accept: %d %s", response.Code, response.Body.String())
	}
	exhausted, _ := accept(t, h, bob, secret)
	unknownSecret, _, _ := memberships.NewSecret()
	unknown, _ := accept(t, h, bob, unknownSecret)
	malformed, _ := accept(t, h, bob, "not-a-secret")
	for name, r := range map[string]*httptest.ResponseRecorder{"exhausted": exhausted, "unknown": unknown, "malformed": malformed} {
		if r.Code != http.StatusNotFound || r.Body.String() != exhausted.Body.String() {
			t.Fatalf("%s: %d %s", name, r.Code, r.Body.String())
		}
	}

	_, created, _ = createInvitation(t, h, key, policy("ADMIN", time.Hour, 2))
	multi := created["secret"].(string)
	if response, _ := accept(t, h, alice, multi); response.Code != http.StatusConflict {
		t.Fatalf("existing member: %d", response.Code)
	}
	for _, token := range []string{bob, carol} {
		if response, _ := accept(t, h, token, multi); response.Code != http.StatusCreated {
			t.Fatalf("multi-use: %d", response.Code)
		}
	}
	if response, _ := accept(t, h, sessionFor(t, h, "dave"), multi); response.Code != http.StatusNotFound {
		t.Fatalf("over max uses: %d", response.Code)
	}
	if len(repo.members) != 3 {
		t.Fatalf("members = %d", len(repo.members))
	}
}

func TestExpiredInvitationAndDisabledUserFail(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	h := invitationServer(repo)
	alice := sessionFor(t, h, "alice")
	_, created, _ := createInvitation(t, h, key, policy("MEMBER", time.Hour, 5))
	secret := created["secret"].(string)
	digest, _ := memberships.SecretDigest(secret)
	repo.invitations[digest].expiresAt = time.Now().Add(-time.Second)
	if response, body := accept(t, h, alice, secret); response.Code != http.StatusNotFound || body["error"] != memberships.ErrUnavailable.Error() {
		t.Fatalf("expired: %d %s", response.Code, response.Body.String())
	}
	repo.invitations[digest].expiresAt = time.Now().Add(time.Hour)
	if _, err := repo.SetUserStatus(context.Background(), repo.byName["alice"].user.ID, users.StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if response, _ := accept(t, h, alice, secret); response.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user: %d", response.Code)
	}
	if len(repo.members) != 0 || repo.invitations[digest].uses != 0 {
		t.Fatal("failed acceptance created membership or consumed a use")
	}
}

func signedJSON(t *testing.T, h http.Handler, method, path, body, nodeID string, key ed25519.PrivateKey) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	response := httptest.NewRecorder()
	h.ServeHTTP(response, signedRequest(method, path, body, nodeID, key, fmt.Sprint(time.Now().Unix())))
	var decoded map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response, decoded
}

func TestSameUserGetsUnrelatedMemberIDsAcrossNodes(t *testing.T) {
	repo, key1 := newMemoryInvitations(t)
	node2 := "node_" + strings.Repeat("c", 64)
	pub2, key2, _ := ed25519.GenerateKey(rand.Reader)
	repo.otherNodes = map[string][]byte{node2: pub2}
	h := invitationServer(repo)
	alice, bob := sessionFor(t, h, "alice"), sessionFor(t, h, "bob")
	aliceUserID := repo.byName["alice"].user.ID

	secretFor := func(nodeID string, key ed25519.PrivateKey) string {
		response, body := signedJSON(t, h, "POST", fmt.Sprintf(invitationsPath, nodeID), policy("MEMBER", time.Hour, 5), nodeID, key)
		if response.Code != http.StatusCreated {
			t.Fatalf("invite %s: %d %s", nodeID, response.Code, response.Body.String())
		}
		return body["secret"].(string)
	}
	secret1, secret2 := secretFor(enrollmentID, key1), secretFor(node2, key2)
	_, on1 := accept(t, h, alice, secret1)
	_, on2 := accept(t, h, alice, secret2)
	_, bobOn1 := accept(t, h, bob, secret1)
	a1, a2, b1 := on1["member_id"].(string), on2["member_id"].(string), bobOn1["member_id"].(string)
	if !memberships.ValidMemberID(a1) || !memberships.ValidMemberID(a2) || a1 == a2 || a1 == b1 ||
		strings.Contains(a1, aliceUserID[5:]) || strings.Contains(a2, aliceUserID[5:]) {
		t.Fatalf("member IDs a1=%s a2=%s b1=%s", a1, a2, b1)
	}

}
