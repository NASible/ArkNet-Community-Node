package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/assertions"
	"arknet/core/internal/memberships"
	"arknet/core/internal/users"
)

// memberRole reports the pairwise member's role while the user is ACTIVE.
func (m *memoryInvitations) memberRole(nodeID, memberID string) (memberships.Role, bool) {
	m.imu.Lock()
	defer m.imu.Unlock()
	m.memoryUsers.mu.Lock()
	defer m.memoryUsers.mu.Unlock()
	for key, member := range m.members {
		if member.MemberID != memberID || !strings.HasPrefix(key, nodeID+"|") {
			continue
		}
		user := m.byID[strings.TrimPrefix(key, nodeID+"|")]
		return member.Role, user != nil && user.user.Status == users.StatusActive
	}
	return "", false
}

func (m *memoryInvitations) AuthorizationStates(_ context.Context, nodeID string, memberIDs []string) ([]memberships.AuthorizationState, error) {
	states := make([]memberships.AuthorizationState, 0, len(memberIDs))
	for _, id := range memberIDs {
		state := memberships.AuthorizationState{MemberID: id}
		if role, ok := m.memberRole(nodeID, id); ok {
			state.Authorized, state.Role = true, role
		}
		states = append(states, state)
	}
	return states, nil
}

func globalMember(t *testing.T, repo *memoryInvitations, username string) string {
	t.Helper()
	h := invitationServer(repo)
	sessionFor(t, h, username)
	repo.memoryUsers.mu.Lock()
	userID := repo.byName[username].user.ID
	repo.memoryUsers.mu.Unlock()
	memberID, _ := memberships.NewMemberID()
	repo.members[enrollmentID+"|"+userID] = memberships.Member{MemberID: memberID, Role: memberships.RoleMember, JoinedAt: time.Now()}
	return memberID
}

func signedGlobalAuthorization(t *testing.T, h http.Handler, key []byte, memberID, password string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"member_id": memberID, "password": password})
	request := signedRequest("POST", fmt.Sprintf("/api/federation/v1/nodes/%s/offline-authorizations", enrollmentID), string(body), enrollmentID, key, fmt.Sprint(time.Now().Unix()))
	response, decoded := callRequest(t, h, request)
	return response.Code, decoded
}

func callRequest(t *testing.T, h http.Handler, request *http.Request) (*responseMeta, string) {
	t.Helper()
	recorder := newResponseMeta()
	h.ServeHTTP(recorder, request)
	return recorder, recorder.Body.String()
}

type responseMeta struct{ *responseRecorder }

type responseRecorder struct {
	HeaderMap http.Header
	Body      strings.Builder
	Code      int
}

func newResponseMeta() *responseMeta {
	return &responseMeta{&responseRecorder{HeaderMap: make(http.Header)}}
}
func (r *responseRecorder) Header() http.Header  { return r.HeaderMap }
func (r *responseRecorder) WriteHeader(code int) { r.Code = code }
func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.Code == 0 {
		r.Code = http.StatusOK
	}
	return r.Body.Write(data)
}

// ARKSEC-001: Core no longer accepts a global password relayed by a Node, so a
// malicious Node cannot use Core as a password oracle.
func TestNodeRelayedPasswordAuthorizationIsRetired(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	memberID := globalMember(t, repo, "offline-user")
	h := invitationServer(repo)
	for _, password := range []string{testPassword, testPassword + "!"} {
		code, body := signedGlobalAuthorization(t, h, key, memberID, password)
		if code != http.StatusGone || strings.Contains(body, memberID) || strings.Contains(body, "role") {
			t.Fatalf("relayed password: %d %s", code, body)
		}
	}
}

func TestGlobalAuthorizationRefreshKeepsTheOfflineWindow(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	memberID := globalMember(t, repo, "window-user")
	h := invitationServer(repo)
	body, _ := json.Marshal(map[string]any{"member_ids": []string{memberID}})
	request := signedRequest("POST", fmt.Sprintf("/api/federation/v1/nodes/%s/offline-authorizations/refresh", enrollmentID), string(body), enrollmentID, key, fmt.Sprint(time.Now().Unix()))
	response, raw := callRequest(t, h, request)
	var decoded struct {
		Authorizations []refreshedAuthorizationView `json:"authorizations"`
	}
	if response.Code != http.StatusOK || json.Unmarshal([]byte(raw), &decoded) != nil || len(decoded.Authorizations) != 1 {
		t.Fatalf("refresh: %d %s", response.Code, raw)
	}
	view := decoded.Authorizations[0]
	validated, e1 := time.Parse(time.RFC3339, view.ValidatedAt)
	expires, e2 := time.Parse(time.RFC3339, view.ExpiresAt)
	// The Agent applies refreshes as shorten-only, so a 24-hour answer would
	// cut every 30-day grant (D3) to one day on reconnect.
	if e1 != nil || e2 != nil || !view.Authorized || expires.Sub(validated) != assertions.MaxOfflineWindow {
		t.Fatalf("refresh window: %+v", view)
	}
}

func TestGlobalAuthorizationRefreshRevokesDisabledMember(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	memberID := globalMember(t, repo, "refresh-user")
	h := invitationServer(repo)
	repo.memoryUsers.mu.Lock()
	userID := repo.byName["refresh-user"].user.ID
	repo.memoryUsers.mu.Unlock()
	if _, err := repo.SetUserStatus(context.Background(), userID, users.StatusDisabled); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"member_ids": []string{memberID}})
	request := signedRequest("POST", fmt.Sprintf("/api/federation/v1/nodes/%s/offline-authorizations/refresh", enrollmentID), string(body), enrollmentID, key, fmt.Sprint(time.Now().Unix()))
	response, raw := callRequest(t, h, request)
	if response.Code != http.StatusOK || !strings.Contains(raw, `"authorized":false`) || strings.Contains(raw, "permissions") {
		t.Fatalf("refresh: %d %s", response.Code, raw)
	}
}
