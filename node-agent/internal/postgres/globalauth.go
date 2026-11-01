package postgres

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"arknet/node-agent/internal/globalauth"
)

var _ globalauth.Repository = (*Store)(nil)

func (s *Store) AssertionKeys(ctx context.Context) ([]globalauth.VerificationKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_id, public_key FROM core_assertion_keys ORDER BY position`)
	if err != nil {
		return nil, fmt.Errorf("read assertion keys: %w", err)
	}
	defer rows.Close()
	var keys []globalauth.VerificationKey
	for rows.Next() {
		var id, public []byte
		if err := rows.Scan(&id, &public); err != nil {
			return nil, err
		}
		key := globalauth.VerificationKey{PublicKey: ed25519.PublicKey(public)}
		copy(key.ID[:], id)
		if len(id) != 8 || globalauth.KeyIDFor(key.PublicKey) != key.ID {
			return nil, errors.New("read assertion keys: stored key does not match its ID")
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) ReplaceAssertionKeys(ctx context.Context, keys []globalauth.VerificationKey) error {
	if len(keys) < 1 || len(keys) > globalauth.MaxAssertionKeys {
		return globalauth.ErrInvalidResponse
	}
	for _, key := range keys {
		if len(key.PublicKey) != ed25519.PublicKeySize || globalauth.KeyIDFor(key.PublicKey) != key.ID {
			return globalauth.ErrInvalidResponse
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM core_assertion_keys`); err != nil {
		return err
	}
	for i, key := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO core_assertion_keys (key_id, public_key, position) VALUES ($1, $2, $3)`,
			key.ID[:], []byte(key.PublicKey), i+1); err != nil {
			return fmt.Errorf("store assertion keys: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) BeginLogin(ctx context.Context, challenge, nonce [32]byte, ttl time.Duration) error {
	if ttl <= 0 || ttl > globalauth.PendingLoginTTL {
		return errors.New("invalid pending login lifetime")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize starts so the bound below holds under concurrency.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE global_login_pending IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM global_login_pending WHERE expires_at <= now()`); err != nil {
		return err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM global_login_pending`).Scan(&pending); err != nil {
		return err
	}
	if pending >= globalauth.MaxPendingLogins {
		return globalauth.ErrBusy
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO global_login_pending (challenge, nonce, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3))`,
		challenge[:], nonce[:], ttl.Seconds()); err != nil {
		return fmt.Errorf("begin global login: %w", err)
	}
	return tx.Commit()
}

func (s *Store) PendingLoginNonce(ctx context.Context, challenge [32]byte) ([32]byte, error) {
	var nonce []byte
	err := s.db.QueryRowContext(ctx, `SELECT nonce FROM global_login_pending WHERE challenge = $1 AND expires_at > now()`, challenge[:]).Scan(&nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return [32]byte{}, globalauth.ErrNotFound
	}
	if err != nil {
		return [32]byte{}, err
	}
	var out [32]byte
	copy(out[:], nonce)
	return out, nil
}

func (s *Store) CompleteLogin(ctx context.Context, login globalauth.CompletedLogin) (time.Time, error) {
	g := login.Grant
	if !globalauth.ValidGrant(g, time.Now()) || login.SessionTTL <= 0 {
		return time.Time{}, globalauth.ErrInvalidResponse
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM global_login_pending WHERE challenge = $1 AND expires_at > now()`, login.Challenge[:])
	if err != nil {
		return time.Time{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return time.Time{}, globalauth.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM global_login_assertions_used WHERE expires_at <= now()`); err != nil {
		return time.Time{}, err
	}
	result, err = tx.ExecContext(ctx, `INSERT INTO global_login_assertions_used (assertion_id, expires_at) VALUES ($1, $2)
		ON CONFLICT (assertion_id) DO NOTHING`, login.AssertionID[:], login.AssertionExpiresAt.Add(globalauth.AssertionClockSkew))
	if err != nil {
		return time.Time{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return time.Time{}, globalauth.ErrReplay
	}
	result, err = tx.ExecContext(ctx, `
		INSERT INTO cached_global_authorizations (member_id, node_id, role, permissions, issued_at, offline_until, assertion)
		SELECT $1, $2, $3, ARRAY['MEDIA_ACCESS']::text[], $4, $5, $6 FROM node_identity WHERE singleton AND public_id = $2
		ON CONFLICT (member_id) DO UPDATE SET role = EXCLUDED.role, issued_at = EXCLUDED.issued_at,
			offline_until = EXCLUDED.offline_until, assertion = EXCLUDED.assertion
		WHERE cached_global_authorizations.node_id = EXCLUDED.node_id
		  AND cached_global_authorizations.issued_at <= EXCLUDED.issued_at`,
		g.MemberID, g.NodeID, g.Role, g.IssuedAt, g.OfflineUntil, g.Assertion)
	if err != nil {
		return time.Time{}, fmt.Errorf("store global grant: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return time.Time{}, globalauth.ErrInvalidResponse
	}
	if login.ReplacedDevice != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM global_device_credentials WHERE digest = $1 AND member_id = $2`, login.ReplacedDevice[:], g.MemberID); err != nil {
			return time.Time{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO global_device_credentials (digest, member_id) VALUES ($1, $2)`, login.DeviceDigest[:], g.MemberID); err != nil {
		return time.Time{}, fmt.Errorf("store device credential: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM global_device_credentials WHERE member_id = $1 AND digest NOT IN (
		SELECT digest FROM global_device_credentials WHERE member_id = $1 ORDER BY created_at DESC, digest LIMIT $2)`,
		g.MemberID, globalauth.MaxDevicesPerMember); err != nil {
		return time.Time{}, err
	}
	expires, err := insertGlobalSession(ctx, tx, g.MemberID, login.SessionDigest, login.SessionTTL, false)
	if err != nil {
		return time.Time{}, err
	}
	return expires, tx.Commit()
}

// offlineGrantQuery selects a still-valid grant for a device credential. The
// clock check refuses offline login if this Node's clock is earlier than the
// newest accepted issued_at minus the skew (a rollback).
const offlineGrantQuery = `
	SELECT a.member_id, a.node_id, a.role, a.issued_at, a.offline_until, a.assertion
	FROM global_device_credentials d
	JOIN cached_global_authorizations a ON a.member_id = d.member_id
	JOIN node_identity n ON n.singleton AND n.public_id = a.node_id
	WHERE d.digest = $1 AND a.offline_until > now()
	  AND now() >= (SELECT max(issued_at) FROM cached_global_authorizations) - interval '60 seconds'`

func scanGrant(row *sql.Row) (globalauth.Grant, error) {
	var g globalauth.Grant
	err := row.Scan(&g.MemberID, &g.NodeID, &g.Role, &g.IssuedAt, &g.OfflineUntil, &g.Assertion)
	return g, err
}

func (s *Store) OfflineGrant(ctx context.Context, deviceDigest [32]byte) (globalauth.Grant, error) {
	g, err := scanGrant(s.db.QueryRowContext(ctx, offlineGrantQuery, deviceDigest[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return globalauth.Grant{}, globalauth.ErrNotFound
	}
	if err != nil {
		return globalauth.Grant{}, fmt.Errorf("read offline grant: %w", err)
	}
	return g, nil
}

func (s *Store) CreateOfflineSession(ctx context.Context, deviceDigest, sessionDigest [32]byte, ttl time.Duration) (time.Time, error) {
	if ttl <= 0 {
		return time.Time{}, globalauth.ErrInactive
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	g, err := scanGrant(tx.QueryRowContext(ctx, offlineGrantQuery+` FOR UPDATE OF a`, deviceDigest[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, globalauth.ErrInactive
	}
	if err != nil {
		return time.Time{}, err
	}
	expires, err := insertGlobalSession(ctx, tx, g.MemberID, sessionDigest, ttl, true)
	if err != nil {
		return time.Time{}, err
	}
	return expires, tx.Commit()
}

// insertGlobalSession creates a session that never outlives the grant.
func insertGlobalSession(ctx context.Context, tx *sql.Tx, memberID string, digest [32]byte, ttl time.Duration, offline bool) (time.Time, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM cached_global_sessions WHERE member_id = $1 AND expires_at <= now()`, memberID); err != nil {
		return time.Time{}, err
	}
	var expires time.Time
	err := tx.QueryRowContext(ctx, `INSERT INTO cached_global_sessions (token_digest, member_id, permissions, offline, expires_at)
		SELECT $1, member_id, ARRAY['MEDIA_ACCESS']::text[], $3, LEAST(now() + make_interval(secs => $4), offline_until)
		FROM cached_global_authorizations WHERE member_id = $2 AND offline_until > now()
		RETURNING expires_at`, digest[:], memberID, offline, ttl.Seconds()).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, globalauth.ErrInactive
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("create global session: %w", err)
	}
	return expires, nil
}

func (s *Store) RevokeAuthorization(ctx context.Context, memberID string) error {
	if !globalauth.ValidMemberID(memberID) {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM cached_global_authorizations WHERE member_id=$1", memberID); err != nil {
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
	if _, err = tx.ExecContext(ctx, "DELETE FROM cached_global_authorizations WHERE offline_until <= now()"); err != nil {
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

// ApplyRefresh applies Core's reconnect refresh. It can only shorten a grant,
// lower ADMIN to MEMBER, or revoke; raising offline_until needs a new online
// login (D3). Any change ends the member's existing sessions.
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
		if state.Authorized && !globalauth.ValidRefresh(state.Role, state.Permissions, state.ValidatedAt, state.ExpiresAt, now) {
			return globalauth.ErrInvalidResponse
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
		// A refreshed expiry at or before issued_at leaves no valid window.
		if _, err := tx.ExecContext(ctx, `DELETE FROM cached_global_authorizations WHERE member_id=$1 AND node_id=$2 AND issued_at >= $3`,
			state.MemberID, nodeID, state.ExpiresAt); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE cached_global_authorizations
			SET offline_until = LEAST(offline_until, $3), role = CASE WHEN $4 = 'MEMBER' THEN 'MEMBER' ELSE role END
			WHERE member_id = $1 AND node_id = $2 AND (offline_until > $3 OR (role = 'ADMIN' AND $4 = 'MEMBER'))`,
			state.MemberID, nodeID, state.ExpiresAt, state.Role)
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

func (s *Store) GetGlobalSession(ctx context.Context, digest [32]byte) (globalauth.Session, error) {
	var session globalauth.Session
	err := s.db.QueryRowContext(ctx, `SELECT s.member_id, s.offline, s.expires_at FROM cached_global_sessions s
		JOIN cached_global_authorizations a ON a.member_id = s.member_id
		WHERE s.token_digest = $1 AND s.expires_at > now() AND a.offline_until > now()`, digest[:]).Scan(&session.MemberID, &session.Offline, &session.ExpiresAt)
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
