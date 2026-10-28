package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"arknet/node-agent/internal/globalauth"
)

func cachedAuthorization(t *testing.T, s *Store, memberID string, expires time.Time) globalauth.Authorization {
	t.Helper()
	snapshot, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	return globalauth.Authorization{MemberID: memberID, NodeID: snapshot.NodeID, PasswordHash: localTestHash, Role: "MEMBER", Permissions: []string{globalauth.PermissionMediaAccess}, ValidatedAt: now, ExpiresAt: expires}
}

func TestCachedGlobalAuthorizationIsBoundedAndContainsNoGlobalIdentity(t *testing.T) {
	s := initializedStore(t)
	ctx := context.Background()
	id := "member_" + repeatHex("a", 64)
	a := cachedAuthorization(t, s, id, time.Now().Add(time.Hour))
	if err := s.CacheAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.CachedAuthorization(ctx, id)
	if err != nil || got.MemberID != id || got.NodeID != a.NodeID || got.PasswordHash != localTestHash {
		t.Fatalf("%+v %v", got, err)
	}
	var prohibited int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='cached_global_authorizations' AND column_name IN ('username','user_id','global_user_id')`).Scan(&prohibited); err != nil || prohibited != 0 {
		t.Fatalf("global identity columns=%d %v", prohibited, err)
	}
	for _, statement := range []string{
		`UPDATE cached_global_authorizations SET password_hash='plaintext-password'`,
		`UPDATE cached_global_authorizations SET permissions=ARRAY['ADMIN']::text[]`,
		`UPDATE cached_global_authorizations SET expires_at=validated_at+interval '25 hours'`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted %s", statement)
		}
	}
	other := a
	other.MemberID = "member_" + repeatHex("b", 64)
	other.NodeID = "node_" + repeatHex("f", 64)
	if err := s.CacheAuthorization(ctx, other); !errors.Is(err, globalauth.ErrInvalidResponse) {
		t.Fatalf("wrong node cached: %v", err)
	}
}

func TestCachedGlobalSessionExpiresAtAuthorizationAndRefreshRevokes(t *testing.T) {
	s := initializedStore(t)
	ctx := context.Background()
	id := "member_" + repeatHex("c", 64)
	a := cachedAuthorization(t, s, id, time.Now().Add(30*time.Minute))
	if err := s.CacheAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	_, digest, _ := globalauth.NewSessionToken()
	expires, err := s.CreateGlobalSession(ctx, id, digest, globalauth.SessionTTL, true)
	if err != nil || expires.After(a.ExpiresAt.Add(time.Second)) {
		t.Fatalf("%v %v", expires, err)
	}
	session, err := s.GetGlobalSession(ctx, digest)
	if err != nil || !session.Offline || session.MemberID != id {
		t.Fatalf("%+v %v", session, err)
	}
	refresh := globalauth.RefreshState{MemberID: id, Authorized: true, Role: "ADMIN", Permissions: []string{globalauth.PermissionMediaAccess}, ValidatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.ApplyRefresh(ctx, a.NodeID, []globalauth.RefreshState{refresh}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGlobalSession(ctx, digest); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatal("refresh did not revoke old session")
	}
	got, _ := s.CachedAuthorization(ctx, id)
	if got.Role != "ADMIN" {
		t.Fatalf("role not refreshed: %+v", got)
	}
	_, fresh, _ := globalauth.NewSessionToken()
	if _, err := s.CreateGlobalSession(ctx, id, fresh, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRefresh(ctx, a.NodeID, []globalauth.RefreshState{{MemberID: id, Authorized: false}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CachedAuthorization(ctx, id); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatal("revoked authorization survived")
	}
	if _, err := s.GetGlobalSession(ctx, fresh); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatal("revoked session survived")
	}
}

func TestExpiredGlobalAuthorizationFailsAndPrunes(t *testing.T) {
	s := initializedStore(t)
	ctx := context.Background()
	id := "member_" + repeatHex("d", 64)
	a := cachedAuthorization(t, s, id, time.Now().Add(time.Hour))
	if err := s.CacheAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE cached_global_authorizations SET validated_at=now()-interval '2 hours',expires_at=now()-interval '1 second' WHERE member_id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CachedAuthorization(ctx, id); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatal("expired cache accepted")
	}
	ids, err := s.CachedMemberIDs(ctx, globalauth.MaxCachedMembers)
	if err != nil || len(ids) != 0 {
		t.Fatalf("expired cache not pruned: %v %v", ids, err)
	}
}

func repeatHex(value string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += value
	}
	return result
}
