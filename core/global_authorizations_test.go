package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/memberships"
	"arknet/core/internal/users"
)

func (m *memoryInvitations) OfflineCredentials(_ context.Context, nodeID, memberID string) (memberships.OfflineCredentials, error) {
	m.imu.Lock()
	defer m.imu.Unlock()
	m.memoryUsers.mu.Lock()
	defer m.memoryUsers.mu.Unlock()
	for key, member := range m.members {
		if member.MemberID != memberID || !strings.HasPrefix(key, nodeID+"|") {
			continue
		}
		user := m.byID[strings.TrimPrefix(key, nodeID+"|")]
		if user == nil || user.user.Status != users.StatusActive {
			break
		}
		return memberships.OfflineCredentials{MemberID: memberID, PasswordHash: user.hash, Role: member.Role}, nil
	}
	return memberships.OfflineCredentials{}, memberships.ErrAuthorizationDenied
}

func (m *memoryInvitations) AuthorizationStates(_ context.Context, nodeID string, memberIDs []string) ([]memberships.AuthorizationState, error) {
	states := make([]memberships.AuthorizationState, 0, len(memberIDs))
	for _, id := range memberIDs {
		credentials, err := m.OfflineCredentials(context.Background(), nodeID, id)
		state := memberships.AuthorizationState{MemberID: id}
		if err == nil {
			state.Authorized, state.Role = true, credentials.Role
		} else if !errors.Is(err, memberships.ErrAuthorizationDenied) {
			return nil, err
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

func TestGlobalAuthorizationUsesPairwiseIdentityAndGenericDenials(t *testing.T) {
	repo, key := newMemoryInvitations(t)
	memberID := globalMember(t, repo, "offline-user")
	h := invitationServer(repo)

	code, body := signedGlobalAuthorization(t, h, key, memberID, testPassword)
	if code != http.StatusOK {
		t.Fatalf("valid login: %d %s", code, body)
	}
	var view offlineAuthorizationView
	if json.Unmarshal([]byte(body), &view) != nil || view.MemberID != memberID || view.NodeID != enrollmentID || view.Role != memberships.RoleMember ||
		len(view.Permissions) != 1 || view.Permissions[0] != offlinePermissionMediaAccess || !view.ExpiresAtAfter(view.ValidatedAt) {
		t.Fatalf("invalid response: %s", body)
	}
	if strings.Contains(body, "user_") || strings.Contains(body, "offline-user") || strings.Contains(body, "$argon2") || strings.Contains(body, testPassword) {
		t.Fatalf("global identity or verifier leaked: %s", body)
	}

	var denied string
	for _, attempt := range [][2]string{{memberID, testPassword + "!"}, {"member_" + strings.Repeat("f", 64), testPassword}, {"bad", "short"}} {
		status, response := signedGlobalAuthorization(t, h, key, attempt[0], attempt[1])
		if status != http.StatusUnauthorized {
			t.Fatalf("%v: %d %s", attempt, status, response)
		}
		if denied == "" {
			denied = response
		} else if response != denied {
			t.Fatalf("distinguishable denial: %q vs %q", response, denied)
		}
	}
}

func (v offlineAuthorizationView) ExpiresAtAfter(validated string) bool {
	expires, e1 := time.Parse(time.RFC3339, v.ExpiresAt)
	at, e2 := time.Parse(time.RFC3339, validated)
	return e1 == nil && e2 == nil && expires.Sub(at) == offlineAuthorizationTTL
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
