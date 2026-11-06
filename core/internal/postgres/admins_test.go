package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"arknet/core/internal/admins"
)

func TestAdminTokensStoreOnlyDigestsAndAuthenticateLiveTokensOnly(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	_, digest, _ := admins.NewToken()
	token, err := s.CreateToken(ctx, "ops", []admins.Permission{admins.UsersAdmin, admins.NodesDecide}, time.Hour, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !admins.ValidTokenID(token.TokenID) || !admins.ValidAdminID(token.AdminID) || token.AdminName != "ops" ||
		token.ExpiresAt.Sub(token.CreatedAt) != time.Hour {
		t.Fatalf("token %+v", token)
	}
	p, err := s.Authenticate(ctx, digest)
	if err != nil || p.TokenID != token.TokenID || p.AdminID != token.AdminID || p.AdminName != "ops" ||
		len(p.Permissions) != 2 || p.Permissions[0] != admins.NodesDecide || p.Permissions[1] != admins.UsersAdmin {
		t.Fatalf("principal %+v, %v", p, err)
	}
	// The same administrator gets one identity across tokens.
	_, second, _ := admins.NewToken()
	other, err := s.CreateToken(ctx, "ops", []admins.Permission{admins.MediaAdmin}, time.Hour, second)
	if err != nil || other.AdminID != token.AdminID || other.TokenID == token.TokenID {
		t.Fatalf("second token %+v %v", other, err)
	}
	// Unknown digest.
	_, unknown, _ := admins.NewToken()
	if _, err := s.Authenticate(ctx, unknown); !errors.Is(err, admins.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	// Revoked.
	if err := s.RevokeToken(ctx, token.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, digest); !errors.Is(err, admins.ErrNotFound) {
		t.Fatalf("revoked: %v", err)
	}
	if err := s.RevokeToken(ctx, token.TokenID); !errors.Is(err, admins.ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
	}
	// Expired: the lookup compares with the database clock.
	if _, err := s.db.ExecContext(ctx, "UPDATE admin_tokens SET created_at=now()-interval '2 hours', expires_at=now()-interval '1 second' WHERE public_id=$1", other.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, second); !errors.Is(err, admins.ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
	// Minting and revoking are themselves audited.
	var minted, revoked int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE outcome='token_minted'), count(*) FILTER (WHERE outcome='token_revoked')
		FROM admin_audit_log WHERE token_id=$1`, token.TokenID).Scan(&minted, &revoked); err != nil || minted != 1 || revoked != 1 {
		t.Fatalf("lifecycle audit minted=%d revoked=%d err=%v", minted, revoked, err)
	}
	// Only the digest is stored.
	var columns string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(column_name, ',' ORDER BY column_name) FROM information_schema.columns WHERE table_name='admin_tokens' AND table_schema=current_schema()`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != "admin_id,created_at,expires_at,id,permissions,public_id,revoked_at,token_digest" {
		t.Fatalf("admin_tokens columns = %s", columns)
	}
}

func TestAdminTokenPolicyIsEnforcedByCodeAndSchema(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	_, digest, _ := admins.NewToken()
	for _, bad := range []struct {
		name  string
		perms []admins.Permission
		ttl   time.Duration
	}{
		{"ops", nil, time.Hour}, {"ops", []admins.Permission{"root"}, time.Hour},
		{"ops", []admins.Permission{admins.UsersAdmin, admins.UsersAdmin}, time.Hour},
		{"ops", []admins.Permission{admins.UsersAdmin}, admins.MaxTTL + time.Hour},
		{"ops", []admins.Permission{admins.UsersAdmin}, 0}, {"Bad Name", []admins.Permission{admins.UsersAdmin}, time.Hour},
	} {
		if _, err := s.CreateToken(ctx, bad.name, bad.perms, bad.ttl, digest); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := s.CreateToken(ctx, "seed", []admins.Permission{admins.MediaAdmin}, time.Hour, digest); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO admin_tokens (public_id, admin_id, token_digest, permissions, expires_at) SELECT 'admtok_' || repeat('a',64), id, '\x00'::bytea, ARRAY['users.admin'], now()+interval '1 hour' FROM administrators LIMIT 1`,
		`INSERT INTO admin_tokens (public_id, admin_id, token_digest, permissions, expires_at) SELECT 'admtok_' || repeat('b',64), id, decode(repeat('ab',32),'hex'), ARRAY['root'], now()+interval '1 hour' FROM administrators LIMIT 1`,
		`INSERT INTO admin_tokens (public_id, admin_id, token_digest, permissions, expires_at) SELECT 'admtok_' || repeat('c',64), id, decode(repeat('cd',32),'hex'), ARRAY['users.admin'], now()+interval '91 days' FROM administrators LIMIT 1`,
		`INSERT INTO admin_tokens (public_id, admin_id, token_digest, permissions, expires_at) SELECT 'admtok_' || repeat('d',64), id, decode(repeat('ef',32),'hex'), ARRAY[]::text[], now()+interval '1 hour' FROM administrators LIMIT 1`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted: %s", statement)
		}
	}
}

func TestAdminAuditIsAppendOnlyAndAttributable(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	_, digest, _ := admins.NewToken()
	token, err := s.CreateToken(ctx, "auditor", []admins.Permission{admins.UsersAdmin}, time.Hour, digest)
	if err != nil {
		t.Fatal(err)
	}
	entry := admins.AuditEntry{AdminID: token.AdminID, TokenID: token.TokenID, Permission: admins.UsersAdmin,
		Action: "POST /api/admin/v1/users/{user_id}/disable", Target: "user_" + repeatHex("a"), Outcome: admins.OutcomeAuthorized}
	if err := s.AppendAudit(ctx, entry); err != nil {
		t.Fatal(err)
	}
	entry.Outcome, entry.Status = admins.OutcomeCompleted, 200
	if err := s.AppendAudit(ctx, entry); err != nil {
		t.Fatal(err)
	}
	var rows int
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), max(a.name) FROM admin_audit_log l JOIN administrators a ON a.public_id=l.admin_id
		WHERE l.token_id=$1 AND l.action LIKE 'POST %'`, token.TokenID).Scan(&rows, &name); err != nil || rows < 2 || name != "auditor" {
		t.Fatalf("audit rows %d name %q err %v", rows, name, err)
	}
	for _, statement := range []string{"UPDATE admin_audit_log SET outcome='denied'", "DELETE FROM admin_audit_log", "TRUNCATE admin_audit_log"} {
		if _, err := s.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("audit log allowed: %s", statement)
		}
	}
	for _, bad := range []admins.AuditEntry{
		{AdminID: token.AdminID, TokenID: token.TokenID, Permission: "root", Action: "x", Outcome: admins.OutcomeDenied},
		{AdminID: token.AdminID, TokenID: token.TokenID, Permission: admins.UsersAdmin, Action: "x", Outcome: "maybe"},
		{AdminID: token.AdminID, TokenID: token.TokenID, Permission: admins.UsersAdmin, Action: "x", Target: "password=hunter2", Outcome: admins.OutcomeDenied},
	} {
		if err := s.AppendAudit(ctx, bad); err == nil {
			t.Fatalf("audit accepted %+v", bad)
		}
	}
}

func repeatHex(c string) string {
	out := ""
	for i := 0; i < 64; i++ {
		out += c
	}
	return out
}
