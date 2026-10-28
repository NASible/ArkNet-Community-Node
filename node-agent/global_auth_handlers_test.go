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

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/globalauth"
)

type memoryGlobalAuth struct {
	mu       sync.Mutex
	auth     map[string]globalauth.Authorization
	sessions map[[32]byte]globalauth.Session
}

func newMemoryGlobalAuth() *memoryGlobalAuth {
	return &memoryGlobalAuth{auth: map[string]globalauth.Authorization{}, sessions: map[[32]byte]globalauth.Session{}}
}
func (m *memoryGlobalAuth) CacheAuthorization(_ context.Context, a globalauth.Authorization) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auth[a.MemberID] = a
	for d, s := range m.sessions {
		if s.MemberID == a.MemberID {
			delete(m.sessions, d)
		}
	}
	return nil
}
func (m *memoryGlobalAuth) CachedAuthorization(_ context.Context, id string) (globalauth.Authorization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.auth[id]
	if !ok || !time.Now().Before(a.ExpiresAt) {
		return globalauth.Authorization{}, globalauth.ErrNotFound
	}
	return a, nil
}
func (m *memoryGlobalAuth) RevokeAuthorization(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.auth, id)
	for d, s := range m.sessions {
		if s.MemberID == id {
			delete(m.sessions, d)
		}
	}
	return nil
}
func (m *memoryGlobalAuth) CachedMemberIDs(context.Context, int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := []string{}
	for id, a := range m.auth {
		if time.Now().Before(a.ExpiresAt) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
func (m *memoryGlobalAuth) ApplyRefresh(_ context.Context, node string, states []globalauth.RefreshState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range states {
		if !s.Authorized {
			delete(m.auth, s.MemberID)
		} else {
			a := m.auth[s.MemberID]
			a.NodeID = node
			a.Role = s.Role
			a.Permissions = s.Permissions
			a.ValidatedAt = s.ValidatedAt
			a.ExpiresAt = s.ExpiresAt
			m.auth[s.MemberID] = a
		}
		for d, x := range m.sessions {
			if x.MemberID == s.MemberID {
				delete(m.sessions, d)
			}
		}
	}
	return nil
}
func (m *memoryGlobalAuth) CreateGlobalSession(_ context.Context, id string, d [32]byte, ttl time.Duration, offline bool) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.auth[id]
	if !ok || !time.Now().Before(a.ExpiresAt) {
		return time.Time{}, globalauth.ErrInactive
	}
	expires := time.Now().Add(ttl)
	if expires.After(a.ExpiresAt) {
		expires = a.ExpiresAt
	}
	m.sessions[d] = globalauth.Session{MemberID: id, Permissions: a.Permissions, Offline: offline, ExpiresAt: expires}
	return expires, nil
}
func (m *memoryGlobalAuth) GetGlobalSession(_ context.Context, d [32]byte) (globalauth.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[d]
	if !ok || !time.Now().Before(s.ExpiresAt) {
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

func globalHandler(repo *memoryGlobalAuth, authorize func(context.Context, enrollment.Identity, string, string) (globalauth.Authorization, error)) http.Handler {
	identity := func(context.Context) (enrollment.Identity, error) {
		return enrollment.Identity{NodeID: "node_" + strings.Repeat("a", 64), CoreURL: "https://core.invalid", Status: "APPROVED", PrivateSeed: make([]byte, 32)}, nil
	}
	return newHandler(testRepository{}, globalAuthOpt{repository: repo, identity: identity, authorize: authorize})
}
func globalLogin(t *testing.T, h http.Handler, id, password string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"member_id": id, "password": password})
	return localCall(t, h, "POST", "/api/v1/global-users/sessions", string(body))
}

func TestGlobalAuthorizationOnlineThenOffline(t *testing.T) {
	repo := newMemoryGlobalAuth()
	memberID := "member_" + strings.Repeat("b", 64)
	password := "global correct horse battery"
	calls := 0
	authorize := func(_ context.Context, _ enrollment.Identity, id, pw string) (globalauth.Authorization, error) {
		calls++
		if calls > 1 {
			return globalauth.Authorization{}, globalauth.ErrUnavailable
		}
		if id != memberID || pw != password {
			return globalauth.Authorization{}, globalauth.ErrDenied
		}
		now := time.Now()
		return globalauth.Authorization{MemberID: id, NodeID: "node_" + strings.Repeat("a", 64), Role: "MEMBER", Permissions: []string{globalauth.PermissionMediaAccess}, ValidatedAt: now, ExpiresAt: now.Add(time.Hour)}, nil
	}
	h := globalHandler(repo, authorize)
	w, online := globalLogin(t, h, memberID, password)
	if w.Code != http.StatusCreated || online["kind"] != "GLOBAL" || online["offline"] != false {
		t.Fatalf("online: %d %s", w.Code, w.Body)
	}
	firstToken := online["token"].(string)
	w, offline := globalLogin(t, h, memberID, password)
	if w.Code != http.StatusCreated || offline["offline"] != true {
		t.Fatalf("offline: %d %s", w.Code, w.Body)
	}
	if offline["token"] == firstToken {
		t.Fatal("session token reused")
	}
	w, current := localCall(t, h, "GET", "/api/v1/global-users/session", "", "Authorization", "Bearer "+offline["token"].(string))
	if w.Code != http.StatusOK || current["member_id"] != memberID || current["offline"] != true {
		t.Fatalf("session: %d %s", w.Code, w.Body)
	}
}

func TestGlobalAuthorizationOfflineRejectsUnknownWrongAndExpired(t *testing.T) {
	repo := newMemoryGlobalAuth()
	memberID := "member_" + strings.Repeat("c", 64)
	password := "another correct horse battery"
	hash, _ := globalauth.HashPassword(context.Background(), password)
	now := time.Now()
	repo.auth[memberID] = globalauth.Authorization{MemberID: memberID, NodeID: "node_" + strings.Repeat("a", 64), PasswordHash: hash, Role: "MEMBER", Permissions: []string{globalauth.PermissionMediaAccess}, ValidatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	h := globalHandler(repo, func(context.Context, enrollment.Identity, string, string) (globalauth.Authorization, error) {
		return globalauth.Authorization{}, globalauth.ErrUnavailable
	})
	var denial string
	for _, attempt := range [][2]string{{"member_" + strings.Repeat("d", 64), password}, {memberID, password + "!"}} {
		w, _ := globalLogin(t, h, attempt[0], attempt[1])
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%v: %d", attempt, w.Code)
		}
		if denial == "" {
			denial = w.Body.String()
		} else if denial != w.Body.String() {
			t.Fatal("distinguishable offline denial")
		}
	}
	expiredID := "member_" + strings.Repeat("e", 64)
	repo.auth[expiredID] = globalauth.Authorization{MemberID: expiredID, NodeID: "node_" + strings.Repeat("a", 64), PasswordHash: hash, Role: "MEMBER", Permissions: []string{globalauth.PermissionMediaAccess}, ValidatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	if w, _ := globalLogin(t, h, expiredID, password); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired: %d", w.Code)
	}
}

func TestAuthoritativeDenialRevokesAndMalformedCoreNeverFallsBack(t *testing.T) {
	for _, coreErr := range []error{globalauth.ErrDenied, globalauth.ErrInvalidResponse} {
		repo := newMemoryGlobalAuth()
		memberID := "member_" + strings.Repeat("f", 64)
		password := "global password material"
		hash, _ := globalauth.HashPassword(context.Background(), password)
		now := time.Now()
		repo.auth[memberID] = globalauth.Authorization{MemberID: memberID, NodeID: "node_" + strings.Repeat("a", 64), PasswordHash: hash, Role: "MEMBER", Permissions: []string{globalauth.PermissionMediaAccess}, ValidatedAt: now, ExpiresAt: now.Add(time.Hour)}
		h := globalHandler(repo, func(context.Context, enrollment.Identity, string, string) (globalauth.Authorization, error) {
			return globalauth.Authorization{}, coreErr
		})
		w, _ := globalLogin(t, h, memberID, password)
		if errors.Is(coreErr, globalauth.ErrDenied) {
			if w.Code != http.StatusUnauthorized || repo.auth[memberID].MemberID != "" {
				t.Fatalf("denial: %d", w.Code)
			}
		} else if w.Code != http.StatusServiceUnavailable || repo.auth[memberID].MemberID == "" {
			t.Fatalf("malformed fallback/revoke: %d", w.Code)
		}
	}
}
