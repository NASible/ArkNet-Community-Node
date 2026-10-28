package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
	"arknet/core/internal/users"
)

var _ memberships.HomeNodeRepository = (*Store)(nil)

// SetHomeNode is the only writer. One statement both verifies that the
// target is the user's own membership on an approved, trusted Node and
// upserts the single per-user row, so a concurrent change cannot produce two
// home nodes or point at another user's membership.
func (s *Store) SetHomeNode(ctx context.Context, userID, nodeID string) (memberships.HomeNode, error) {
	if !users.ValidUserID(userID) || !nodes.ValidPublicID(nodeID) {
		return memberships.HomeNode{}, memberships.ErrHomeNodeUnavailable
	}
	var home memberships.HomeNode
	err := s.db.QueryRowContext(ctx, `
WITH target AS (
    SELECT m.id AS membership_id, m.user_id, m.public_id AS member_id, n.public_id AS node_id
    FROM node_memberships m
    JOIN nodes n ON n.id = m.node_id
    JOIN global_users u ON u.id = m.user_id
    WHERE u.public_id = $1 AND u.status = 'ACTIVE'
      AND n.public_id = $2 AND n.enrollment_status = 'APPROVED' AND n.trust_status = 'TRUSTED'
), upserted AS (
    INSERT INTO global_user_home_nodes (user_id, membership_id)
    SELECT user_id, membership_id FROM target
    ON CONFLICT (user_id) DO UPDATE
    SET membership_id = EXCLUDED.membership_id, updated_at = now()
    RETURNING updated_at
)
SELECT t.node_id, t.member_id, u.updated_at FROM target t, upserted u`, userID, nodeID).
		Scan(&home.NodeID, &home.MemberID, &home.SetAt)
	if errors.Is(err, sql.ErrNoRows) {
		return memberships.HomeNode{}, memberships.ErrHomeNodeUnavailable
	}
	if err != nil {
		return memberships.HomeNode{}, fmt.Errorf("set home node: %w", err)
	}
	return home, nil
}

// HomeNode returns the user's preferred Node for Core routing logic. A home
// node on a Node that is no longer approved/trusted, or for a disabled user,
// is reported as absent; Core never falls back to another membership.
func (s *Store) HomeNode(ctx context.Context, userID string) (memberships.HomeNode, error) {
	if !users.ValidUserID(userID) {
		return memberships.HomeNode{}, memberships.ErrNoHomeNode
	}
	var home memberships.HomeNode
	err := s.db.QueryRowContext(ctx, `
SELECT n.public_id, m.public_id, h.updated_at
FROM global_user_home_nodes h
JOIN global_users u ON u.id = h.user_id
JOIN node_memberships m ON m.id = h.membership_id AND m.user_id = h.user_id
JOIN nodes n ON n.id = m.node_id
WHERE u.public_id = $1 AND u.status = 'ACTIVE'
  AND n.enrollment_status = 'APPROVED' AND n.trust_status = 'TRUSTED'`, userID).
		Scan(&home.NodeID, &home.MemberID, &home.SetAt)
	if errors.Is(err, sql.ErrNoRows) {
		return memberships.HomeNode{}, memberships.ErrNoHomeNode
	}
	if err != nil {
		return memberships.HomeNode{}, fmt.Errorf("get home node: %w", err)
	}
	return home, nil
}
