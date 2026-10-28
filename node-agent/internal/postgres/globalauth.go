package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"arknet/node-agent/internal/globalauth"
)

var _ globalauth.Repository = (*Store)(nil)

func (s *Store) CacheAuthorization(ctx context.Context, a globalauth.Authorization) error {
	if !globalauth.ValidAuthorization(a, time.Now()) {
		return globalauth.ErrInvalidResponse
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO cached_global_authorizations (member_id, node_id, password_hash, role, permissions, validated_at, expires_at)
		SELECT $1,$2,$3,$4,$5,$6,$7 FROM node_identity WHERE singleton AND public_id = $2
		ON CONFLICT (member_id) DO UPDATE SET node_id=EXCLUDED.node_id, password_hash=EXCLUDED.password_hash,
		role=EXCLUDED.role, permissions=EXCLUDED.permissions, validated_at=EXCLUDED.validated_at, expires_at=EXCLUDED.expires_at
		WHERE cached_global_authorizations.node_id = EXCLUDED.node_id
		  AND cached_global_authorizations.validated_at <= EXCLUDED.validated_at`,
		a.MemberID, a.NodeID, a.PasswordHash, a.Role, a.Permissions, a.ValidatedAt, a.ExpiresAt)
	if err != nil {
		return fmt.Errorf("cache global authorization: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return globalauth.ErrInvalidResponse
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM cached_global_sessions WHERE member_id=$1", a.MemberID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CachedAuthorization(ctx context.Context, memberID string) (globalauth.Authorization, error) {
	if !globalauth.ValidMemberID(memberID) {
		return globalauth.Authorization{}, globalauth.ErrNotFound
	}
	var a globalauth.Authorization
	err := s.db.QueryRowContext(ctx, `SELECT member_id,node_id,password_hash,role,validated_at,expires_at
		FROM cached_global_authorizations WHERE member_id=$1 AND expires_at > now()`, memberID).
		Scan(&a.MemberID, &a.NodeID, &a.PasswordHash, &a.Role, &a.ValidatedAt, &a.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return globalauth.Authorization{}, globalauth.ErrNotFound
	}
	if err != nil {
		return globalauth.Authorization{}, fmt.Errorf("get cached global authorization: %w", err)
	}
	a.Permissions = []string{globalauth.PermissionMediaAccess}
	return a, nil
}

func (s *Store) RevokeAuthorization(ctx context.Context, memberID string) error {
	if !globalauth.ValidMemberID(memberID) {
		return nil
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM cached_global_authorizations WHERE member_id=$1", memberID)
	if err != nil {
		return fmt.Errorf("revoke global authorization: %w", err)
	}
	return nil
}

func (s *Store) CachedMemberIDs(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > globalauth.MaxCachedMembers {
		return nil, errors.New("invalid cached-member limit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM cached_global_authorizations WHERE expires_at <= now()"); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT member_id FROM cached_global_authorizations ORDER BY member_id LIMIT $1", limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *Store) ApplyRefresh(ctx context.Context, nodeID string, states []globalauth.RefreshState) error {
	if !globalauth.ValidNodeID(nodeID) || len(states) < 1 || len(states) > globalauth.MaxCachedMembers {
		return globalauth.ErrInvalidResponse
	}
	seen := map[string]bool{}
	now := time.Now()
	for _, state := range states {
		if !globalauth.ValidMemberID(state.MemberID) || seen[state.MemberID] {
			return globalauth.ErrInvalidResponse
		}
		seen[state.MemberID] = true
		if state.Authorized {
			probe := globalauth.Authorization{MemberID: state.MemberID, NodeID: nodeID, PasswordHash: "present", Role: state.Role, Permissions: state.Permissions, ValidatedAt: state.ValidatedAt, ExpiresAt: state.ExpiresAt}
			if !globalauth.ValidAuthorization(probe, now) {
				return globalauth.ErrInvalidResponse
			}
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var own string
	if err := tx.QueryRowContext(ctx, "SELECT public_id FROM node_identity WHERE singleton").Scan(&own); err != nil {
		return err
	}
	if own != nodeID {
		return globalauth.ErrInvalidResponse
	}
	for _, state := range states {
		if !state.Authorized {
			if _, err := tx.ExecContext(ctx, "DELETE FROM cached_global_authorizations WHERE member_id=$1 AND node_id=$2", state.MemberID, nodeID); err != nil {
				return err
			}
			continue
		}
		result, err := tx.ExecContext(ctx, `UPDATE cached_global_authorizations SET role=$3,permissions=$4,validated_at=$5,expires_at=$6
			WHERE member_id=$1 AND node_id=$2 AND validated_at <= $5`, state.MemberID, nodeID, state.Role, state.Permissions, state.ValidatedAt, state.ExpiresAt)
		if err != nil {
			return fmt.Errorf("refresh global authorization: %w", err)
		}
		if n, _ := result.RowsAffected(); n == 1 {
			if _, err := tx.ExecContext(ctx, "DELETE FROM cached_global_sessions WHERE member_id=$1", state.MemberID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) CreateGlobalSession(ctx context.Context, memberID string, digest [32]byte, ttl time.Duration, offline bool) (time.Time, error) {
	if !globalauth.ValidMemberID(memberID) || ttl <= 0 {
		return time.Time{}, globalauth.ErrInactive
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	var expires time.Time
	err = tx.QueryRowContext(ctx, `SELECT LEAST(now()+make_interval(secs=>$2),expires_at) FROM cached_global_authorizations
		WHERE member_id=$1 AND expires_at>now() FOR UPDATE`, memberID, ttl.Seconds()).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, globalauth.ErrInactive
	}
	if err != nil {
		return time.Time{}, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM cached_global_sessions WHERE member_id=$1 AND expires_at<=now()", memberID); err != nil {
		return time.Time{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO cached_global_sessions(token_digest,member_id,permissions,offline,expires_at) VALUES($1,$2,$3,$4,$5)`, digest[:], memberID, []string{globalauth.PermissionMediaAccess}, offline, expires)
	if err != nil {
		return time.Time{}, fmt.Errorf("create global session: %w", err)
	}
	return expires, tx.Commit()
}

func (s *Store) GetGlobalSession(ctx context.Context, digest [32]byte) (globalauth.Session, error) {
	var session globalauth.Session
	err := s.db.QueryRowContext(ctx, `SELECT s.member_id,s.offline,s.expires_at FROM cached_global_sessions s
		JOIN cached_global_authorizations a ON a.member_id=s.member_id
		WHERE s.token_digest=$1 AND s.expires_at>now() AND a.expires_at>now()`, digest[:]).Scan(&session.MemberID, &session.Offline, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return globalauth.Session{}, globalauth.ErrNotFound
	}
	if err != nil {
		return globalauth.Session{}, err
	}
	session.Permissions = []string{globalauth.PermissionMediaAccess}
	return session, nil
}
func (s *Store) DeleteGlobalSession(ctx context.Context, digest [32]byte) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM cached_global_sessions WHERE token_digest=$1", digest[:])
	return err
}
