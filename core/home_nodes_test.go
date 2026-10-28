package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
	"arknet/core/internal/users"
)

// memoryHomes follows the HomeNodeRepository contract over memoryInvitations.
type memoryHomes struct {
	*memoryInvitations
	homes map[string]memberships.HomeNode
	sets  int
}

func (m *memoryHomes) trusted(nodeID string) bool {
	if _, ok := m.otherNodes[nodeID]; ok {
		return true
	}
	return nodeID == m.node.PublicID && m.node.EnrollmentStatus == nodes.EnrollmentApproved && m.node.TrustStatus == nodes.TrustTrusted
}

func (m *memoryHomes) active(userID string) bool {
	m.memoryUsers.mu.Lock()
	defer m.memoryUsers.mu.Unlock()
	user := m.byID[userID]
	return user != nil && user.user.Status == users.StatusActive
}

func (m *memoryHomes) SetHomeNode(_ context.Context, userID, nodeID string) (memberships.HomeNode, error) {
	m.sets++
	m.imu.Lock()
	member, ok := m.members[nodeID+"|"+userID]
	m.imu.Unlock()
	if !ok || !m.trusted(nodeID) || !m.active(userID) {
		return memberships.HomeNode{}, memberships.ErrHomeNodeUnavailable
	}
	home := memberships.HomeNode{NodeID: nodeID, MemberID: member.MemberID, SetAt: time.Now()}
	m.homes[userID] = home
	return home, nil
}

func (m *memoryHomes) HomeNode(_ context.Context, userID string) (memberships.HomeNode, error) {
	home, ok := m.homes[userID]
	if !ok || !m.trusted(home.NodeID) || !m.active(userID) {
		return memberships.HomeNode{}, memberships.ErrNoHomeNode
	}
	return home, nil
}

type homeFixture struct {
	repo         *memoryHomes
	h            http.Handler
	key1, key2   ed25519.PrivateKey
	node2        string
	alice, bob   string
	aliceID      string
	a1, a2, bob1 string
}

func newHomeFixture(t *testing.T) homeFixture {
	t.Helper()
	invitations, key1 := newMemoryInvitations(t)
	f := homeFixture{repo: &memoryHomes{memoryInvitations: invitations, homes: map[string]memberships.HomeNode{}}, key1: key1}
	f.node2 = "node_" + strings.Repeat("c", 64)
	pub2, key2, _ := ed25519.GenerateKey(rand.Reader)
	f.key2 = key2
	f.repo.otherNodes = map[string][]byte{f.node2: pub2}
	f.h = withAdministration(newHandler(healthyDatabase{}, f.repo), f.repo, allowAdmin(true))
	f.alice, f.bob = sessionFor(t, f.h, "alice"), sessionFor(t, f.h, "bob")
	f.aliceID = f.repo.byName["alice"].user.ID
	secretFor := func(nodeID string, key ed25519.PrivateKey) string {
		response, body := signedJSON(t, f.h, "POST", fmt.Sprintf(invitationsPath, nodeID), policy("MEMBER", time.Hour, 5), nodeID, key)
		if response.Code != http.StatusCreated {
			t.Fatalf("invite: %d %s", response.Code, response.Body)
		}
		return body["secret"].(string)
	}
	s1, s2 := secretFor(enrollmentID, key1), secretFor(f.node2, key2)
	_, on1 := accept(t, f.h, f.alice, s1)
	_, on2 := accept(t, f.h, f.alice, s2)
	_, b1 := accept(t, f.h, f.bob, s1)
	f.a1, f.a2, f.bob1 = on1["member_id"].(string), on2["member_id"].(string), b1["member_id"].(string)
	return f
}

func homeCall(t *testing.T, h http.Handler, method, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	if token == "" {
		return call(t, h, method, "/api/identity/v1/home-node", body)
	}
	return call(t, h, method, "/api/identity/v1/home-node", body, "Authorization", "Bearer "+token)
}

func TestHomeNodeRequiresExplicitChangeAmongOwnMemberships(t *testing.T) {
	f := newHomeFixture(t)
	// Joining several Nodes never selects a home node implicitly.
	if response, _ := homeCall(t, f.h, "GET", f.alice, ""); response.Code != http.StatusNotFound {
		t.Fatalf("implicit home: %d %s", response.Code, response.Body)
	}
	if f.repo.sets != 0 {
		t.Fatal("invitation acceptance changed the home node")
	}
	response, body := homeCall(t, f.h, "PUT", f.alice, `{"node_id":"`+f.node2+`"}`)
	if response.Code != http.StatusOK || body["node_id"] != f.node2 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("set: %d %s", response.Code, response.Body)
	}
	var keys []string
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "node_id,set_at" {
		t.Fatalf("user view exposes extra fields: %s", response.Body)
	}
	response, body = homeCall(t, f.h, "PUT", f.alice, `{"node_id":"`+enrollmentID+`"}`)
	if response.Code != http.StatusOK || body["node_id"] != enrollmentID {
		t.Fatalf("change: %d %s", response.Code, response.Body)
	}
	if _, body := homeCall(t, f.h, "GET", f.alice, ""); body["node_id"] != enrollmentID {
		t.Fatalf("one home node expected: %v", body)
	}
	// Bob is not a member of node2: same response as an unknown Node.
	notMember, _ := homeCall(t, f.h, "PUT", f.bob, `{"node_id":"`+f.node2+`"}`)
	unknown, _ := homeCall(t, f.h, "PUT", f.bob, `{"node_id":"node_`+strings.Repeat("d", 64)+`"}`)
	if notMember.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound || notMember.Body.String() != unknown.Body.String() {
		t.Fatalf("membership probe distinguishable: %d %s / %d %s", notMember.Code, notMember.Body, unknown.Code, unknown.Body)
	}
	if _, body := homeCall(t, f.h, "GET", f.bob, ""); body["node_id"] != nil {
		t.Fatalf("failed change set a home node: %v", body)
	}
}

func TestHomeNodeRoutesRejectUnauthenticatedAndInexactRequests(t *testing.T) {
	f := newHomeFixture(t)
	for _, method := range []string{"GET", "PUT"} {
		if response, _ := homeCall(t, f.h, method, "", `{"node_id":"`+enrollmentID+`"}`); response.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated: %d", method, response.Code)
		}
		if response, _ := homeCall(t, f.h, method, "not-a-token", `{"node_id":"`+enrollmentID+`"}`); response.Code != http.StatusUnauthorized {
			t.Fatalf("%s bad token: %d", method, response.Code)
		}
	}
	for _, body := range []string{``, `{}`, `null`, `{"node_id":1}`, `{"node_id":"bad"}`,
		`{"node_id":"` + enrollmentID + `","user_id":"x"}`, `{"node_id":"` + enrollmentID + `"}{}`} {
		if response, _ := homeCall(t, f.h, "PUT", f.alice, body); response.Code != http.StatusBadRequest {
			t.Fatalf("%q: %d", body, response.Code)
		}
	}
	if f.repo.sets != 0 {
		t.Fatal("invalid request reached storage")
	}
	// A Node signature is not a user session and cannot read or set home nodes.
	for _, method := range []string{"GET", "PUT"} {
		response := httptest.NewRecorder()
		f.h.ServeHTTP(response, signedRequest(method, "/api/identity/v1/home-node", `{"node_id":"`+enrollmentID+`"}`, enrollmentID, f.key1, fmt.Sprint(time.Now().Unix())))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("node-signed %s: %d", method, response.Code)
		}
	}
}

func TestRoutingReceivesHomeNodeButNodesCannotEnumerateMemberships(t *testing.T) {
	f := newHomeFixture(t)
	if response, _ := homeCall(t, f.h, "PUT", f.alice, `{"node_id":"`+f.node2+`"}`); response.Code != http.StatusOK {
		t.Fatalf("set: %d", response.Code)
	}
	digest, _ := users.SessionDigest(f.alice)
	home, err := preferredHomeNode(context.Background(), f.repo, f.repo, digest)
	if err != nil || home.NodeID != f.node2 || home.MemberID != f.a2 {
		t.Fatalf("routing home node: %+v %v", home, err)
	}
	if _, err := preferredHomeNode(context.Background(), f.repo, f.repo, [32]byte{1}); !errors.Is(err, errSessionRequired) {
		t.Fatalf("unauthenticated routing lookup: %v", err)
	}

	for _, node := range []struct {
		id  string
		key ed25519.PrivateKey
	}{{enrollmentID, f.key1}, {f.node2, f.key2}} {
		response, _ := signedJSON(t, f.h, "GET", "/api/federation/v1/nodes/"+node.id+"/memberships", "", node.id, node.key)
		if response.Code != http.StatusNotFound {
			t.Fatalf("membership enumeration route for %s: %d %s", node.id, response.Code, response.Body)
		}
	}

	// A home node on a Node that loses trust is not routable and Core does
	// not silently fall back to another membership.
	delete(f.repo.otherNodes, f.node2)
	if response, _ := homeCall(t, f.h, "GET", f.alice, ""); response.Code != http.StatusNotFound {
		t.Fatalf("untrusted home node still routable: %d %s", response.Code, response.Body)
	}
}
