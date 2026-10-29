package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"arknet/node-agent/internal/operators"
)

func digestOf(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func digestSlice(s string) []byte { d := digestOf(s); return d[:] }

func newSecret(t *testing.T) []byte {
	t.Helper()
	secret, err := operators.NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func recoveryDigests(prefix string) [][32]byte {
	out := make([][32]byte, operators.RecoveryCodeCount)
	for i := range out {
		out[i] = digestOf(prefix + string(rune('a'+i)))
	}
	return out
}

// bootstrapOperator runs SETUP code -> enrollment -> confirm and returns the session.
func bootstrapOperator(t *testing.T, s *Store, username string, additional bool) operators.Session {
	t.Helper()
	ctx := context.Background()
	code := digestOf("setup-" + username)
	if _, err := s.IssueSetupCode(ctx, code, additional); err != nil {
		t.Fatal(err)
	}
	enrollment := digestOf("enroll-" + username)
	if _, err := s.BeginEnrollment(ctx, code, username, localTestHash, newSecret(t), enrollment); err != nil {
		t.Fatal(err)
	}
	session, err := s.ConfirmEnrollment(ctx, enrollment, 100, recoveryDigests(username), digestOf("session-"+username))
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestOperatorBootstrapCodeIsSingleUseAndRefusedOnceOperatorsExist(t *testing.T) {
	s := initializedStore(t)
	ctx := context.Background()
	if n, err := s.CountActiveOperators(ctx); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	first, second := digestOf("first"), digestOf("second")
	if _, err := s.IssueSetupCode(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	// Issuing a new code invalidates the earlier unused one.
	if _, err := s.IssueSetupCode(ctx, second, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginEnrollment(ctx, first, "alice", localTestHash, newSecret(t), digestOf("e0")); !errors.Is(err, operators.ErrInvalidCode) {
		t.Fatalf("superseded code accepted: %v", err)
	}
	if _, err := s.BeginEnrollment(ctx, second, "alice", localTestHash, newSecret(t), digestOf("e1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginEnrollment(ctx, second, "mallory", localTestHash, newSecret(t), digestOf("e2")); !errors.Is(err, operators.ErrInvalidCode) {
		t.Fatalf("setup code reused: %v", err)
	}
	if _, err := s.ConfirmEnrollment(ctx, digestOf("e1"), 100, recoveryDigests("a"), digestOf("s1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmEnrollment(ctx, digestOf("e1"), 101, recoveryDigests("a"), digestOf("s2")); !errors.Is(err, operators.ErrInvalidCode) {
		t.Fatalf("enrollment reused: %v", err)
	}
	if n, _ := s.CountActiveOperators(ctx); n != 1 {
		t.Fatalf("operators: %d", n)
	}
	if _, err := s.IssueSetupCode(ctx, digestOf("third"), false); !errors.Is(err, operators.ErrOperatorsExist) {
		t.Fatalf("setup code issued while an operator exists: %v", err)
	}
	if _, err := s.IssueSetupCode(ctx, digestOf("third"), true); err != nil {
		t.Fatalf("--additional: %v", err)
	}
	if _, err := s.BeginEnrollment(ctx, digestOf("third"), "alice", localTestHash, newSecret(t), digestOf("e3")); !errors.Is(err, operators.ErrUsernameTaken) {
		t.Fatalf("duplicate username: %v", err)
	}
	// A refused username does not spend the code.
	if _, err := s.BeginEnrollment(ctx, digestOf("third"), "bob", localTestHash, newSecret(t), digestOf("e3")); err != nil {
		t.Fatal(err)
	}
	// Expired codes are refused.
	if _, err := s.IssueSetupCode(ctx, digestOf("fourth"), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE operator_setup_codes SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE code_digest = $1", digestSlice("fourth")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginEnrollment(ctx, digestOf("fourth"), "carol", localTestHash, newSecret(t), digestOf("e4")); !errors.Is(err, operators.ErrInvalidCode) {
		t.Fatalf("expired code accepted: %v", err)
	}
}

func TestOperatorCredentialsSecondFactorSpendAndFailureDelay(t *testing.T) {
	s := initializedStore(t)
	ctx := context.Background()
	bootstrapOperator(t, s, "alice", false)
	creds, err := s.OperatorCredentials(ctx, "alice")
	if err != nil || creds.PasswordHash != localTestHash || creds.TOTPLastStep != 100 || creds.Status != operators.StatusActive || creds.RetryAfter != nil {
		t.Fatalf("%+v %v", creds, err)
	}
	if _, err := s.OperatorCredentials(ctx, "nobody"); !errors.Is(err, operators.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.CreateOperatorSession(ctx, creds.OperatorID, operators.SecondFactor{TOTPStep: 100}, digestOf("x")); !errors.Is(err, operators.ErrReplay) {
		t.Fatalf("replayed TOTP step: %v", err)
	}
	if _, err := s.CreateOperatorSession(ctx, creds.OperatorID, operators.SecondFactor{TOTPStep: 101}, digestOf("x1")); err != nil {
		t.Fatal(err)
	}
	code := recoveryDigests("alice")[3]
	if _, err := s.CreateOperatorSession(ctx, creds.OperatorID, operators.SecondFactor{RecoveryDigest: &code}, digestOf("x2")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOperatorSession(ctx, creds.OperatorID, operators.SecondFactor{RecoveryDigest: &code}, digestOf("x3")); !errors.Is(err, operators.ErrReplay) {
		t.Fatalf("recovery code reused: %v", err)
	}
	for range operators.FailureThreshold {
		if err := s.RecordFailedLogin(ctx, creds.OperatorID); err != nil {
			t.Fatal(err)
		}
	}
	if creds, _ = s.OperatorCredentials(ctx, "alice"); creds.RetryAfter == nil {
		t.Fatal("no delay after repeated failures")
	}
	// A successful sign-in clears the counter.
	if _, err := s.CreateOperatorSession(ctx, creds.OperatorID, operators.SecondFactor{TOTPStep: 102}, digestOf("x4")); err != nil {
		t.Fatal(err)
	}
	if creds, _ = s.OperatorCredentials(ctx, "alice"); creds.RetryAfter != nil {
		t.Fatal("delay survived a successful sign-in")
	}
	if err := s.DisableOperator(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOperatorSession(ctx, creds.OperatorID, operators.SecondFactor{TOTPStep: 103}, digestOf("x5")); !errors.Is(err, operators.ErrInactive) {
		t.Fatalf("disabled operator signed in: %v", err)
	}
	if _, err := s.GetOperatorSession(ctx, digestOf("x4")); !errors.Is(err, operators.ErrNotFound) {
		t.Fatal("disable did not revoke sessions")
	}
}

func TestOperatorSessionIdleAndAbsoluteExpiryAndLogout(t *testing.T) {
	s := initializedStore(t)
	ctx := context.Background()
	session := bootstrapOperator(t, s, "alice", false)
	digest := digestOf("session-alice")
	if session.ExpiresAt.Sub(session.AuthenticatedAt) < operators.SessionTTL-1e9 || !session.IdleExpiresAt.Before(session.ExpiresAt) {
		t.Fatalf("session %+v", session)
	}
	got, err := s.GetOperatorSession(ctx, digest)
	if err != nil || got.Username != "alice" || !operators.ValidOperatorID(got.OperatorID) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE operator_sessions SET idle_expires_at = now() - interval '1 second' WHERE token_digest = $1", digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOperatorSession(ctx, digest); !errors.Is(err, operators.ErrNotFound) {
		t.Fatalf("idle-expired session accepted: %v", err)
	}
	creds, _ := s.OperatorCredentials(ctx, "alice")
	if _, err := s.CreateOperatorSession(ctx, creds.OperatorID, operators.SecondFactor{TOTPStep: 200}, digestOf("abs")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE operator_sessions SET created_at = now() - interval '13 hours',
		expires_at = now() - interval '1 second', idle_expires_at = now() - interval '1 second' WHERE token_digest = $1`, digestSlice("abs")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOperatorSession(ctx, digestOf("abs")); !errors.Is(err, operators.ErrNotFound) {
		t.Fatal("absolutely expired session accepted")
	}
	// Use extends idle expiry, never past the absolute expiry.
	if _, err := s.CreateOperatorSession(ctx, creds.OperatorID, operators.SecondFactor{TOTPStep: 201}, digestOf("live")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE operator_sessions SET expires_at = now() + interval '5 minutes', idle_expires_at = now() + interval '1 minute' WHERE token_digest = $1", digestSlice("live")); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetOperatorSession(ctx, digestOf("live"))
	if err != nil || !got.IdleExpiresAt.Equal(got.ExpiresAt) {
		t.Fatalf("idle extension not capped at absolute expiry: %+v %v", got, err)
	}
	for range 2 { // logout is idempotent
		if err := s.DeleteOperatorSession(ctx, digestOf("live")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetOperatorSession(ctx, digestOf("live")); !errors.Is(err, operators.ErrNotFound) {
		t.Fatal("logged-out session accepted")
	}
	for i := range operators.MaxSessionsPerOperator + 2 {
		if _, err := s.CreateOperatorSession(ctx, creds.OperatorID, operators.SecondFactor{TOTPStep: int64(300 + i)}, digestOf("cap"+string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM operator_sessions").Scan(&count); err != nil || count != operators.MaxSessionsPerOperator {
		t.Fatalf("sessions: %d %v", count, err)
	}
	if _, err := s.GetOperatorSession(ctx, digestOf("capa")); !errors.Is(err, operators.ErrNotFound) {
		t.Fatal("oldest session kept past the cap")
	}
}

func TestOperatorShellRecoveryRevokesAndReplacesCredentials(t *testing.T) {
	s := initializedStore(t)
	ctx := context.Background()
	bootstrapOperator(t, s, "alice", false)
	bootstrapOperator(t, s, "bob", true)
	if _, err := s.IssueRecoveryCode(ctx, "nobody", digestOf("r0")); !errors.Is(err, operators.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.IssueRecoveryCode(ctx, "alice", digestOf("r1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOperatorSession(ctx, digestOf("session-alice")); !errors.Is(err, operators.ErrNotFound) {
		t.Fatal("recovery did not revoke sessions immediately")
	}
	if _, err := s.GetOperatorSession(ctx, digestOf("session-bob")); err != nil {
		t.Fatal("recovery revoked another operator's session")
	}
	if _, err := s.BeginEnrollment(ctx, digestOf("r1"), "bob", localTestHash, newSecret(t), digestOf("re0")); !errors.Is(err, operators.ErrInvalidCode) {
		t.Fatalf("recovery code used for another operator: %v", err)
	}
	newHash := strings.Replace(localTestHash, "aGFzaGhhc2g", "bmV3aGFzaGg", 1)
	secret := newSecret(t)
	enrollment, err := s.BeginEnrollment(ctx, digestOf("r1"), "alice", newHash, secret, digestOf("re1"))
	if err != nil || enrollment.Purpose != operators.PurposeRecovery {
		t.Fatalf("%+v %v", enrollment, err)
	}
	if pending, err := s.PendingEnrollment(ctx, digestOf("re1")); err != nil || string(pending.TOTPSecret) != string(secret) {
		t.Fatalf("pending %v", err)
	}
	if _, err := s.ConfirmEnrollment(ctx, digestOf("re1"), 500, recoveryDigests("new"), digestOf("after")); err != nil {
		t.Fatal(err)
	}
	creds, _ := s.OperatorCredentials(ctx, "alice")
	if creds.PasswordHash != newHash || string(creds.TOTPSecret) != string(secret) || creds.TOTPLastStep != 500 {
		t.Fatal("recovery did not replace credentials")
	}
	old := recoveryDigests("alice")[0]
	if _, err := s.CreateOperatorSession(ctx, creds.OperatorID, operators.SecondFactor{RecoveryDigest: &old}, digestOf("y")); !errors.Is(err, operators.ErrReplay) {
		t.Fatal("old recovery code survived recovery")
	}
	if err := s.DisableOperator(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssueRecoveryCode(ctx, "bob", digestOf("r2")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginEnrollment(ctx, digestOf("r2"), "bob", localTestHash, newSecret(t), digestOf("re2")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmEnrollment(ctx, digestOf("re2"), 600, recoveryDigests("bob2"), digestOf("bob-after")); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListOperators(ctx)
	if err != nil || len(list) != 2 || list[1].Username != "bob" || list[1].Status != operators.StatusActive || list[0].LastSignInAt == nil {
		t.Fatalf("%+v %v", list, err)
	}
	var events string
	if err := s.db.QueryRowContext(ctx, "SELECT string_agg(DISTINCT event, ',') FROM operator_events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OPERATOR_CREATED", "OPERATOR_RECOVERED", "OPERATOR_DISABLED", "RECOVERY_CODE_ISSUED", "SETUP_CODE_ISSUED"} {
		if !strings.Contains(events, want) {
			t.Fatalf("missing event %s in %s", want, events)
		}
	}
}

// An existing Node database at migration 0010, with real local state, gains
// the operator tables without data loss and starts with no operator.
func TestMigration0011UpgradesExistingNodeDatabase(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	legacy := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0011" {
			continue
		}
		data, _ := fs.ReadFile(migrationFiles, "migrations/"+entry.Name())
		legacy["migrations/"+entry.Name()] = &fstest.MapFile{Data: data}
	}
	if len(legacy) != 10 {
		t.Fatalf("legacy migrations: %d", len(legacy))
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
	for _, statement := range []string{
		`INSERT INTO node_identity(singleton,public_id) VALUES(true,'node_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')`,
		`INSERT INTO node_configuration(singleton,settings) VALUES(true,'{"peer_serving":true}')`,
		`INSERT INTO node_state(singleton,state) VALUES(true,'CONNECTED')`,
		`INSERT INTO local_users (public_id, username, password_hash) VALUES ('local_` + strings.Repeat("b", 64) + `', 'carol', '` + localTestHash + `')`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.OperatorCredentials(ctx, "alice"); err == nil {
		t.Fatal("operator tables exist before the migration")
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountActiveOperators(ctx); err != nil || n != 0 {
		t.Fatalf("upgraded Node has operators: %d %v", n, err)
	}
	var id, peer, username string
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT public_id FROM node_identity), (SELECT settings->>'peer_serving' FROM node_configuration),
		(SELECT username FROM local_users)`).Scan(&id, &peer, &username); err != nil {
		t.Fatal(err)
	}
	if id != "node_"+strings.Repeat("a", 64) || peer != "true" || username != "carol" {
		t.Fatalf("upgrade changed data: %s %s %s", id, peer, username)
	}
	bootstrapOperator(t, s, "alice", false)
	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("migration replay failed: %v", err)
	}
	for _, statement := range []string{
		"INSERT INTO operators (public_id, username, password_hash, totp_secret) VALUES ('local_x', 'eve', '" + localTestHash + "', decode(repeat('00',20),'hex'))",
		"INSERT INTO operators (public_id, username, password_hash, totp_secret) VALUES ('op_" + strings.Repeat("c", 64) + "', 'eve', 'plaintext', decode(repeat('00',20),'hex'))",
		"INSERT INTO operator_setup_codes (code_digest, purpose, expires_at) VALUES ('\\x00', 'SETUP', now() + interval '1 hour')",
		"INSERT INTO operator_setup_codes (code_digest, purpose, expires_at) VALUES (decode(repeat('00',32),'hex'), 'RECOVERY', now() + interval '1 hour')",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted %q", statement)
		}
	}
}
