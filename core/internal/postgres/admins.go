package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"arknet/core/internal/admins"
)

var _ admins.Repository = (*Store)(nil)

func (s *Store) Authenticate(ctx context.Context, digest [32]byte) (admins.Principal, error) {
	var p admins.Principal
	var permissions string
	err := s.db.QueryRowContext(ctx, `SELECT t.public_id, a.public_id, a.name, array_to_string(t.permissions, ','), t.expires_at
		FROM admin_tokens t JOIN administrators a ON a.id = t.admin_id
		WHERE t.token_digest = $1 AND t.revoked_at IS NULL AND t.expires_at > now()`, digest[:]).
		Scan(&p.TokenID, &p.AdminID, &p.AdminName, &permissions, &p.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return admins.Principal{}, admins.ErrNotFound
	}
	if err != nil {
		return admins.Principal{}, fmt.Errorf("authenticate administrator: %w", err)
	}
	normalized, err := admins.NormalizePermissions(strings.Split(permissions, ","))
	if err != nil {
		return admins.Principal{}, fmt.Errorf("stored administrator permissions invalid")
	}
	p.Permissions = normalized
	return p, nil
}

func (s *Store) CreateToken(ctx context.Context, name string, permissions []admins.Permission, ttl time.Duration, digest [32]byte) (admins.Token, error) {
	if !admins.ValidName(name) {
		return admins.Token{}, admins.ErrInvalidName
	}
	values := make([]string, 0, len(permissions))
	for _, p := range permissions {
		values = append(values, string(p))
	}
	normalized, err := admins.NormalizePermissions(values)
	if err != nil || !admins.ValidTTL(ttl) {
		return admins.Token{}, admins.ErrInvalidPolicy
	}
	values = values[:0]
	for _, p := range normalized {
		values = append(values, string(p))
	}
	adminID, err := admins.NewAdminID()
	if err != nil {
		return admins.Token{}, err
	}
	tokenID, err := admins.NewTokenID()
	if err != nil {
		return admins.Token{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return admins.Token{}, fmt.Errorf("create administrator token: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO administrators (public_id, name) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING", adminID, name); err != nil {
		return admins.Token{}, fmt.Errorf("create administrator: %w", err)
	}
	token := admins.Token{TokenID: tokenID, AdminName: name, Permissions: normalized}
	var internalID int64
	if err := tx.QueryRowContext(ctx, "SELECT id, public_id FROM administrators WHERE name = $1", name).Scan(&internalID, &token.AdminID); err != nil {
		return admins.Token{}, fmt.Errorf("load administrator: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO admin_tokens (public_id, admin_id, token_digest, permissions, created_at, expires_at)
		SELECT $1, $2, $3, string_to_array($4, ','), now(), now() + make_interval(secs => $5)
		RETURNING created_at, expires_at`, tokenID, internalID, digest[:], strings.Join(values, ","), ttl.Seconds()).
		Scan(&token.CreatedAt, &token.ExpiresAt); err != nil {
		return admins.Token{}, fmt.Errorf("store administrator token: %w", err)
	}
	if err := appendAudit(ctx, tx, admins.AuditEntry{AdminID: token.AdminID, TokenID: tokenID, Action: "cli create-token",
		Outcome: admins.OutcomeMinted}); err != nil {
		return admins.Token{}, err
	}
	if err := tx.Commit(); err != nil {
		return admins.Token{}, fmt.Errorf("create administrator token: %w", err)
	}
	return token, nil
}

func (s *Store) RevokeToken(ctx context.Context, tokenID string) error {
	if !admins.ValidTokenID(tokenID) {
		return admins.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("revoke administrator token: %w", err)
	}
	defer tx.Rollback()
	var adminID string
	err = tx.QueryRowContext(ctx, `UPDATE admin_tokens t SET revoked_at = now() FROM administrators a
		WHERE t.public_id = $1 AND t.revoked_at IS NULL AND a.id = t.admin_id RETURNING a.public_id`, tokenID).Scan(&adminID)
	if errors.Is(err, sql.ErrNoRows) {
		return admins.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke administrator token: %w", err)
	}
	if err := appendAudit(ctx, tx, admins.AuditEntry{AdminID: adminID, TokenID: tokenID, Action: "cli revoke-token",
		Outcome: admins.OutcomeRevoked}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AppendAudit(ctx context.Context, entry admins.AuditEntry) error {
	return appendAudit(ctx, s.db, entry)
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func appendAudit(ctx context.Context, db execer, entry admins.AuditEntry) error {
	var permission any
	if entry.Permission != "" {
		permission = string(entry.Permission)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO admin_audit_log (admin_id, token_id, permission, action, target, outcome, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, entry.AdminID, entry.TokenID, permission, entry.Action, entry.Target, entry.Outcome, entry.Status)
	if err != nil {
		return fmt.Errorf("append administrator audit: %w", err)
	}
	return nil
}
