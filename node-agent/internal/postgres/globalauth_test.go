package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"arknet/node-agent/internal/globalauth"
)

type loginFixture struct {
	s      *Store
	nodeID string
}

func newLoginFixture(t *testing.T) loginFixture {
	t.Helper()
	s := initializedStore(t)
	snapshot, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return loginFixture{s: s, nodeID: snapshot.NodeID}
}

func random32() [32]byte {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return b
}

// login begins and completes one online login and returns the session and
// device digests.
func (f loginFixture) login(t *testing.T, memberID, role string, offlineUntil time.Time, replaced *[32]byte) (session, device [32]byte, err error) {
	t.Helper()
	ctx := context.Background()
	challenge, nonce := random32(), random32()
	if err := f.s.BeginLogin(ctx, challenge, nonce, globalauth.PendingLoginTTL); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.PendingLoginNonce(ctx, challenge)
	if err != nil || got != nonce {
		t.Fatalf("pending nonce: %v", err)
	}
	session, device = random32(), random32()
	var id [16]byte
	_, _ = rand.Read(id[:])
	now := time.Now().UTC().Truncate(time.Second)
	_, err = f.s.CompleteLogin(ctx, globalauth.CompletedLogin{Challenge: challenge, AssertionID: id, AssertionExpiresAt: now.Add(2 * time.Minute),
		Grant:        globalauth.Grant{MemberID: memberID, NodeID: f.nodeID, Role: role, IssuedAt: now, OfflineUntil: offlineUntil, Assertion: "signed.assertion"},
		DeviceDigest: device, ReplacedDevice: replaced, SessionDigest: session, SessionTTL: globalauth.SessionTTL})
	return session, device, err
}

func TestMigrationDeletesPasswordVerifiersAndTheirSessions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	legacy := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0012" {
			continue
		}
		data, _ := fs.ReadFile(migrationFiles, "migrations/"+entry.Name())
		legacy["migrations/"+entry.Name()] = &fstest.MapFile{Data: data}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, tx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	member := "member_" + repeatHex("a", 64)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cached_global_authorizations (member_id,node_id,password_hash,role,permissions,validated_at,expires_at)
		VALUES ($1,$2,$3,'MEMBER',ARRAY['MEDIA_ACCESS'],now(),now()+interval '1 hour')`, member, "node_"+repeatHex("f", 64), localTestHash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cached_global_sessions (token_digest,member_id,permissions,offline,expires_at)
		VALUES ($1,$2,ARRAY['MEDIA_ACCESS'],false,now()+interval '1 hour')`, make([]byte, 32), member); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	var grants, sessions, verifierColumns int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM cached_global_authorizations), (SELECT count(*) FROM cached_global_sessions),
		(SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND column_name ILIKE '%password%'
		 AND table_name IN ('cached_global_authorizations','cached_global_sessions','global_device_credentials','global_login_pending'))`).
		Scan(&grants, &sessions, &verifierColumns); err != nil {
		t.Fatal(err)
	}
	if grants != 0 || sessions != 0 || verifierColumns != 0 {
		t.Fatalf("after migration: %d grants, %d sessions, %d password columns", grants, sessions, verifierColumns)
	}
}

func TestCompleteLoginConsumesPendingStateOnceAndCapsTheSession(t *testing.T) {
	f := newLoginFixture(t)
	ctx := context.Background()
	member := "member_" + repeatHex("b", 64)
	offline := time.Now().Add(time.Hour)
	session, _, err := f.login(t, member, "MEMBER", offline, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.GetGlobalSession(ctx, session)
	if err != nil || got.MemberID != member || got.Offline || got.ExpiresAt.After(offline) {
		t.Fatalf("session %+v %v", got, err)
	}
	// The pending login is consumed, and the same assertion ID is refused.
	challenge, nonce := random32(), random32()
	if err := f.s.BeginLogin(ctx, challenge, nonce, globalauth.PendingLoginTTL); err != nil {
		t.Fatal(err)
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	now := time.Now().UTC().Truncate(time.Second)
	done := globalauth.CompletedLogin{Challenge: challenge, AssertionID: id, AssertionExpiresAt: now.Add(time.Minute),
		Grant:        globalauth.Grant{MemberID: member, NodeID: f.nodeID, Role: "MEMBER", IssuedAt: now, OfflineUntil: offline, Assertion: "a.b"},
		DeviceDigest: random32(), SessionDigest: random32(), SessionTTL: globalauth.SessionTTL}
	if _, err := f.s.CompleteLogin(ctx, done); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CompleteLogin(ctx, done); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatalf("pending login reused: %v", err)
	}
	if err := f.s.BeginLogin(ctx, done.Challenge, random32(), globalauth.PendingLoginTTL); err != nil {
		t.Fatal(err)
	}
	done.DeviceDigest, done.SessionDigest = random32(), random32()
	if _, err := f.s.CompleteLogin(ctx, done); !errors.Is(err, globalauth.ErrReplay) {
		t.Fatalf("assertion ID reused: %v", err)
	}
	_, _ = rand.Read(done.AssertionID[:])
	done.Grant.NodeID = "node_" + repeatHex("e", 64)
	if _, err := f.s.CompleteLogin(ctx, done); !errors.Is(err, globalauth.ErrInvalidResponse) {
		t.Fatalf("grant for another Node stored: %v", err)
	}
	expired := random32()
	if err := f.s.BeginLogin(ctx, expired, random32(), globalauth.PendingLoginTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(ctx, `UPDATE global_login_pending SET created_at=now()-interval '6 minutes', expires_at=now()-interval '1 minute' WHERE challenge=$1`, expired[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PendingLoginNonce(ctx, expired); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatalf("expired pending login: %v", err)
	}
	if err := f.s.BeginLogin(ctx, random32(), random32(), 6*time.Minute); err == nil {
		t.Fatal("pending login longer than 5 minutes accepted")
	}
	for _, statement := range []string{
		`UPDATE cached_global_authorizations SET offline_until=issued_at+interval '30 days 1 second'`,
		`UPDATE cached_global_authorizations SET permissions=ARRAY['ADMIN']::text[]`,
	} {
		if _, err := f.s.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted %s", statement)
		}
	}
}

func TestPendingLoginsAreBounded(t *testing.T) {
	f := newLoginFixture(t)
	ctx := context.Background()
	if _, err := f.s.db.ExecContext(ctx, `INSERT INTO global_login_pending (challenge, nonce, expires_at)
		SELECT sha256(('c'||i)::bytea), sha256(('n'||i)::bytea), now()+interval '5 minutes' FROM generate_series(1,$1::int) i`, globalauth.MaxPendingLogins); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginLogin(ctx, random32(), random32(), globalauth.PendingLoginTTL); !errors.Is(err, globalauth.ErrBusy) {
		t.Fatalf("pending logins unbounded: %v", err)
	}
	if _, err := f.s.db.ExecContext(ctx, `UPDATE global_login_pending SET created_at=now()-interval '6 minutes', expires_at=now()-interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginLogin(ctx, random32(), random32(), globalauth.PendingLoginTTL); err != nil {
		t.Fatalf("expired pending logins not pruned: %v", err)
	}
}

func TestOfflineGrantNeedsDeviceCredentialWindowAndSaneClock(t *testing.T) {
	f := newLoginFixture(t)
	ctx := context.Background()
	member := "member_" + repeatHex("c", 64)
	offline := time.Now().Add(time.Hour)
	_, device, err := f.login(t, member, "MEMBER", offline, nil)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := f.s.OfflineGrant(ctx, device)
	if err != nil || grant.MemberID != member || grant.Assertion != "signed.assertion" || grant.NodeID != f.nodeID {
		t.Fatalf("grant %+v %v", grant, err)
	}
	if _, err := f.s.OfflineGrant(ctx, random32()); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
	session := random32()
	expires, err := f.s.CreateOfflineSession(ctx, device, session, globalauth.SessionTTL)
	if err != nil || expires.After(offline) {
		t.Fatalf("offline session %v %v", expires, err)
	}
	if got, err := f.s.GetGlobalSession(ctx, session); err != nil || !got.Offline {
		t.Fatalf("offline session %+v %v", got, err)
	}
	// Clock rolled back behind the newest accepted issued_at (beyond skew).
	if _, err := f.s.db.ExecContext(ctx, `UPDATE cached_global_authorizations SET issued_at=now()+interval '61 seconds'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.OfflineGrant(ctx, device); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatalf("offline login with a rolled-back clock: %v", err)
	}
	if _, err := f.s.CreateOfflineSession(ctx, device, random32(), globalauth.SessionTTL); !errors.Is(err, globalauth.ErrInactive) {
		t.Fatalf("offline session with a rolled-back clock: %v", err)
	}
	// After the window.
	if _, err := f.s.db.ExecContext(ctx, `UPDATE cached_global_authorizations SET issued_at=now()-interval '2 hours', offline_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.OfflineGrant(ctx, device); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatalf("offline login after the window: %v", err)
	}
	if _, err := f.s.GetGlobalSession(ctx, session); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatalf("session outlived the grant: %v", err)
	}
	if ids, err := f.s.CachedMemberIDs(ctx, globalauth.MaxCachedMembers); err != nil || len(ids) != 0 {
		t.Fatalf("expired grant not pruned: %v %v", ids, err)
	}
}

func TestDeviceCredentialsAreReplacedAndBounded(t *testing.T) {
	f := newLoginFixture(t)
	ctx := context.Background()
	member := "member_" + repeatHex("d", 64)
	offline := time.Now().Add(time.Hour)
	_, first, err := f.login(t, member, "MEMBER", offline, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := f.login(t, member, "MEMBER", offline, &first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.OfflineGrant(ctx, first); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatal("replaced device credential still works")
	}
	if _, err := f.s.OfflineGrant(ctx, second); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < globalauth.MaxDevicesPerMember+2; i++ {
		if _, _, err := f.login(t, member, "MEMBER", offline, nil); err != nil {
			t.Fatal(err)
		}
	}
	var devices int
	if err := f.s.db.QueryRowContext(ctx, `SELECT count(*) FROM global_device_credentials WHERE member_id=$1`, member).Scan(&devices); err != nil || devices != globalauth.MaxDevicesPerMember {
		t.Fatalf("devices %d %v", devices, err)
	}
}

func TestRefreshOnlyShortensOrRevokes(t *testing.T) {
	f := newLoginFixture(t)
	ctx := context.Background()
	member := "member_" + repeatHex("e", 64)
	offline := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	session, device, err := f.login(t, member, "MEMBER", offline, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	authorized := func(role string, expires time.Time) []globalauth.RefreshState {
		return []globalauth.RefreshState{{MemberID: member, Authorized: true, Role: role, Permissions: []string{globalauth.PermissionMediaAccess}, ValidatedAt: now, ExpiresAt: expires}}
	}
	// A later expiry and a higher role never extend the grant.
	if err := f.s.ApplyRefresh(ctx, f.nodeID, authorized("ADMIN", now.Add(30*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	grant, _ := f.s.OfflineGrant(ctx, device)
	if !grant.OfflineUntil.Equal(offline) || grant.Role != "MEMBER" {
		t.Fatalf("refresh extended the grant: %+v", grant)
	}
	if _, err := f.s.GetGlobalSession(ctx, session); err != nil {
		t.Fatalf("no-op refresh revoked the session: %v", err)
	}
	// An earlier expiry shortens it and ends existing sessions.
	shorter := offline.Add(-time.Hour)
	if err := f.s.ApplyRefresh(ctx, f.nodeID, authorized("MEMBER", shorter)); err != nil {
		t.Fatal(err)
	}
	if grant, _ := f.s.OfflineGrant(ctx, device); !grant.OfflineUntil.Equal(shorter) {
		t.Fatalf("refresh did not shorten: %+v", grant)
	}
	if _, err := f.s.GetGlobalSession(ctx, session); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatal("shortened grant kept its sessions")
	}
	if err := f.s.ApplyRefresh(ctx, "node_"+repeatHex("e", 64), []globalauth.RefreshState{{MemberID: member}}); !errors.Is(err, globalauth.ErrInvalidResponse) {
		t.Fatalf("refresh for another Node: %v", err)
	}
	if err := f.s.ApplyRefresh(ctx, f.nodeID, []globalauth.RefreshState{{MemberID: member}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.OfflineGrant(ctx, device); !errors.Is(err, globalauth.ErrNotFound) {
		t.Fatal("revoked grant survived")
	}
	var devices int
	if err := f.s.db.QueryRowContext(ctx, `SELECT count(*) FROM global_device_credentials`).Scan(&devices); err != nil || devices != 0 {
		t.Fatalf("revoked member's device credentials survived: %d %v", devices, err)
	}
}

func TestAssertionKeysAreReplacedAsASet(t *testing.T) {
	f := newLoginFixture(t)
	ctx := context.Background()
	key := func(b byte) globalauth.VerificationKey {
		public := make([]byte, 32)
		public[0] = b
		return globalauth.VerificationKey{ID: globalauth.KeyIDFor(public), PublicKey: public}
	}
	if keys, err := f.s.AssertionKeys(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("initial keys %v %v", keys, err)
	}
	if err := f.s.ReplaceAssertionKeys(ctx, []globalauth.VerificationKey{key(1), key(2)}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReplaceAssertionKeys(ctx, []globalauth.VerificationKey{key(2), key(3)}); err != nil {
		t.Fatal(err)
	}
	keys, err := f.s.AssertionKeys(ctx)
	if err != nil || len(keys) != 2 || keys[0].ID != key(2).ID || keys[1].ID != key(3).ID {
		t.Fatalf("keys %+v %v", keys, err)
	}
	mismatched := key(4)
	mismatched.ID = key(5).ID
	if err := f.s.ReplaceAssertionKeys(ctx, []globalauth.VerificationKey{mismatched}); err == nil {
		t.Fatal("key with a wrong ID stored")
	}
	if err := f.s.ReplaceAssertionKeys(ctx, nil); err == nil {
		t.Fatal("empty key set stored")
	}
}

func repeatHex(value string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += value
	}
	return result
}
