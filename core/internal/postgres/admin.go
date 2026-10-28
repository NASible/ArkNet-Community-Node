package postgres

import (
	"arknet/core/internal/nodes"
	"context"
	"database/sql"
	"errors"
)

var _ nodes.AdministrationRepository = (*Store)(nil)

// Approve atomically promotes only a pending, untrusted node.
func (s *Store) Approve(ctx context.Context, publicID string) (nodes.Node, error) {
	return s.decide(ctx, publicID, nodes.EnrollmentApproved, nodes.TrustTrusted)
}

// Reject atomically blocks only a pending, untrusted node.
func (s *Store) Reject(ctx context.Context, publicID string) (nodes.Node, error) {
	return s.decide(ctx, publicID, nodes.EnrollmentRejected, nodes.TrustBlocked)
}

// Revoke atomically blocks only an approved, trusted node.
func (s *Store) Revoke(ctx context.Context, publicID string) (nodes.Node, error) {
	return s.transition(ctx, publicID, nodes.EnrollmentApproved, nodes.TrustTrusted, nodes.EnrollmentRevoked, nodes.TrustBlocked)
}

func (s *Store) decide(ctx context.Context, publicID string, enrollment nodes.EnrollmentStatus, trust nodes.TrustStatus) (nodes.Node, error) {
	return s.transition(ctx, publicID, nodes.EnrollmentPending, nodes.TrustUntrusted, enrollment, trust)
}

func (s *Store) transition(ctx context.Context, publicID string, fromEnrollment nodes.EnrollmentStatus, fromTrust nodes.TrustStatus, enrollment nodes.EnrollmentStatus, trust nodes.TrustStatus) (nodes.Node, error) {
	if !nodes.ValidPublicID(publicID) {
		return nodes.Node{}, nodes.ErrInvalidPublicID
	}
	if s == nil || s.db == nil {
		return nodes.Node{}, errors.New("administration database unavailable")
	}
	node, err := scanNode(s.db.QueryRowContext(ctx,
		"UPDATE nodes SET enrollment_status=$2, trust_status=$3 WHERE public_id=$1 AND enrollment_status=$4 AND trust_status=$5 RETURNING "+columns,
		publicID, enrollment, trust, fromEnrollment, fromTrust))
	if errors.Is(err, sql.ErrNoRows) {
		// A separate read sees a concurrent winner after the conditional update;
		// it distinguishes missing records without mutating any existing state.
		_, err := s.GetByPublicID(ctx, publicID)
		if errors.Is(err, nodes.ErrNotFound) {
			return nodes.Node{}, nodes.ErrNotFound
		}
		if err != nil {
			return nodes.Node{}, errors.New("read enrollment decision failed")
		}
		return nodes.Node{}, nodes.ErrEnrollmentConflict
	}
	if err != nil {
		return nodes.Node{}, errors.New("update enrollment decision failed")
	}
	return node, nil
}

// ListPending returns one lookahead row beyond limit; callers own cursor emission.
func (s *Store) ListPending(ctx context.Context, after string, limit int) ([]nodes.Node, error) {
	if limit < 1 || limit > 100 {
		return nil, nodes.ErrInvalidPage
	}
	if after != "" && !nodes.ValidPublicID(after) {
		return nil, nodes.ErrInvalidPublicID
	}
	if s == nil || s.db == nil {
		return nil, errors.New("administration database unavailable")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM nodes WHERE enrollment_status=$1 AND trust_status=$2 AND public_id > $3 ORDER BY public_id ASC LIMIT $4", nodes.EnrollmentPending, nodes.TrustUntrusted, after, limit+1)
	if err != nil {
		return nil, errors.New("list pending nodes failed")
	}
	defer rows.Close()
	result := make([]nodes.Node, 0, limit+1)
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, errors.New("read pending node failed")
		}
		result = append(result, node)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("list pending nodes failed")
	}
	return result, nil
}
