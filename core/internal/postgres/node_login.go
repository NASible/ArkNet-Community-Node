package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"arknet/core/internal/assertions"
	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
	"arknet/core/internal/users"
)

var _ assertions.Repository = (*Store)(nil)

// trustedNodeID returns the internal ID of an APPROVED and TRUSTED Node, or
// assertions.ErrDenied.
func trustedNodeID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, publicID string) (int64, error) {
	if !nodes.ValidPublicID(publicID) {
		return 0, assertions.ErrDenied
	}
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM nodes WHERE public_id = $1
		AND enrollment_status = 'APPROVED' AND trust_status = 'TRUSTED'`, publicID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, assertions.ErrDenied
	}
	if err != nil {
		return 0, fmt.Errorf("read login node: %w", err)
	}
	return id, nil
}

func (s *Store) LoginCallbacks(ctx context.Context, nodeID string) ([]string, error) {
	id, err := trustedNodeID(ctx, s.db, nodeID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT callback_url FROM node_login_callbacks WHERE node_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, fmt.Errorf("list login callbacks: %w", err)
	}
	defer rows.Close()
	var urls []string
	for rows.Next() {
		var callback string
		if err := rows.Scan(&callback); err != nil {
			return nil, fmt.Errorf("list login callbacks: %w", err)
		}
		urls = append(urls, callback)
	}
	return urls, rows.Err()
}

// SetLoginCallbacks replaces the Node's callbacks atomically. The handler has
// already validated the URLs; the schema bounds count and length again.
func (s *Store) SetLoginCallbacks(ctx context.Context, nodeID string, callbackURLs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, err := trustedNodeID(ctx, tx, nodeID)
	if err != nil {
		return err
	}
	// Serialize concurrent replacements for one Node.
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM nodes WHERE id = $1 FOR UPDATE`, id); err != nil {
		return fmt.Errorf("replace login callbacks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_login_callbacks WHERE node_id = $1`, id); err != nil {
		return fmt.Errorf("replace login callbacks: %w", err)
	}
	for i, callback := range callbackURLs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_login_callbacks (node_id, position, callback_url) VALUES ($1, $2, $3)`,
			id, i+1, callback); err != nil {
			return fmt.Errorf("replace login callbacks: %w", err)
		}
	}
	return tx.Commit()
}

// LoginMembership returns the user's pairwise identity on an approved, trusted
// Node. Not a member, inactive user and untrusted Node are one ErrDenied.
func (s *Store) LoginMembership(ctx context.Context, nodeID, userID string) (assertions.Membership, error) {
	if !nodes.ValidPublicID(nodeID) || !users.ValidUserID(userID) {
		return assertions.Membership{}, assertions.ErrDenied
	}
	var membership assertions.Membership
	err := s.db.QueryRowContext(ctx, `
		SELECT m.public_id, m.role
		FROM node_memberships m
		JOIN nodes n ON n.id = m.node_id
		JOIN global_users u ON u.id = m.user_id
		WHERE n.public_id = $1 AND u.public_id = $2
		  AND n.enrollment_status = 'APPROVED' AND n.trust_status = 'TRUSTED'
		  AND u.status = 'ACTIVE'`, nodeID, userID).Scan(&membership.MemberID, &membership.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return assertions.Membership{}, assertions.ErrDenied
	}
	if err != nil {
		return assertions.Membership{}, fmt.Errorf("read login membership: %w", err)
	}
	if !memberships.ValidMemberID(membership.MemberID) || !memberships.ValidRole(membership.Role) {
		return assertions.Membership{}, fmt.Errorf("read login membership: invalid stored membership")
	}
	return membership, nil
}

// ClaimLoginNonce records a nonce digest once per Node and prunes expired
// claims. The insert is a single statement, so concurrent claims of one nonce
// yield exactly one success.
func (s *Store) ClaimLoginNonce(ctx context.Context, nodeID string, nonceDigest [32]byte, retention time.Duration) error {
	if retention <= 0 {
		return fmt.Errorf("claim login nonce: retention must be positive")
	}
	id, err := trustedNodeID(ctx, s.db, nodeID)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM node_login_nonces WHERE expires_at <= now()`); err != nil {
		return fmt.Errorf("prune login nonces: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO node_login_nonces (node_id, nonce_digest, expires_at)
		VALUES ($1, $2, now() + make_interval(secs => $3)) ON CONFLICT (node_id, nonce_digest) DO NOTHING`, id, nonceDigest[:], retention.Seconds())
	if err != nil {
		return fmt.Errorf("claim login nonce: %w", err)
	}
	if inserted, err := result.RowsAffected(); err != nil || inserted != 1 {
		if err != nil {
			return fmt.Errorf("claim login nonce: %w", err)
		}
		return assertions.ErrNonceUsed
	}
	return nil
}
