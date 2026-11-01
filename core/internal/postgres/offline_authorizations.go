package postgres

import (
	"context"
	"errors"
	"fmt"

	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
)

// AuthorizationStates returns one result per requested pairwise ID in request
// order. Unknown, removed, or inactive memberships are explicitly unauthorized.
func (s *Store) AuthorizationStates(ctx context.Context, nodeID string, memberIDs []string) ([]memberships.AuthorizationState, error) {
	if !nodes.ValidPublicID(nodeID) || len(memberIDs) < 1 || len(memberIDs) > memberships.MaxAuthorizationRefreshMembers {
		return nil, memberships.ErrAuthorizationDenied
	}
	seen := make(map[string]bool, len(memberIDs))
	for _, id := range memberIDs {
		if !memberships.ValidMemberID(id) || seen[id] {
			return nil, memberships.ErrAuthorizationDenied
		}
		seen[id] = true
	}
	var trusted bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM nodes WHERE public_id = $1 AND enrollment_status = 'APPROVED' AND trust_status = 'TRUSTED')`, nodeID).Scan(&trusted); err != nil {
		return nil, fmt.Errorf("check authorization node: %w", err)
	}
	if !trusted {
		return nil, memberships.ErrNodeNotFound
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH requested(member_id, ordinal) AS (
			SELECT * FROM unnest($2::text[]) WITH ORDINALITY
		)
		SELECT r.member_id,
		       (m.id IS NOT NULL AND u.status = 'ACTIVE') AS authorized,
		       COALESCE(m.role, '')
		FROM requested r
		JOIN nodes n ON n.public_id = $1
		LEFT JOIN node_memberships m ON m.node_id = n.id AND m.public_id = r.member_id
		LEFT JOIN global_users u ON u.id = m.user_id
		ORDER BY r.ordinal`, nodeID, memberIDs)
	if err != nil {
		return nil, fmt.Errorf("refresh authorization states: %w", err)
	}
	defer rows.Close()
	states := make([]memberships.AuthorizationState, 0, len(memberIDs))
	for rows.Next() {
		var state memberships.AuthorizationState
		if err := rows.Scan(&state.MemberID, &state.Authorized, &state.Role); err != nil {
			return nil, fmt.Errorf("scan authorization state: %w", err)
		}
		if !state.Authorized {
			state.Role = ""
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read authorization states: %w", err)
	}
	if len(states) != len(memberIDs) {
		return nil, errors.New("incomplete authorization state response")
	}
	return states, nil
}
