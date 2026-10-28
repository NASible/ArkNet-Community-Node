package postgres

import (
	"arknet/core/internal/nodes"
	"context"
	"errors"
	"fmt"
)

var _ nodes.EnrollmentRepository = (*Store)(nil)

// Enroll inserts an initial untrusted pending node, or returns the unchanged
// existing node. A separate SELECT after INSERT avoids the snapshot visibility
// race of a single-statement CTE when another enrollment wins the unique key.
func (s *Store) Enroll(ctx context.Context, publicID, softwareVersion string) (nodes.Node, error) {
	if !nodes.ValidPublicID(publicID) {
		return nodes.Node{}, nodes.ErrInvalidPublicID
	}
	if !nodes.ValidSoftwareVersion(softwareVersion) {
		return nodes.Node{}, nodes.ErrInvalidSoftwareVersion
	}
	if s == nil || s.db == nil {
		return nodes.Node{}, errors.New("enrollment database unavailable")
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO nodes (public_id, enrollment_status, trust_status, software_version) VALUES ($1,$2,$3,$4) ON CONFLICT (public_id) DO NOTHING",
		publicID, nodes.EnrollmentPending, nodes.TrustUntrusted, softwareVersion)
	if err != nil {
		return nodes.Node{}, fmt.Errorf("enroll node: %w", err)
	}
	node, err := s.GetByPublicID(ctx, publicID)
	if err != nil {
		return nodes.Node{}, err
	}
	if node.EnrollmentStatus != nodes.EnrollmentPending || node.TrustStatus != nodes.TrustUntrusted {
		return nodes.Node{}, nodes.ErrEnrollmentConflict
	}
	return node, nil
}
