package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
	"arknet/core/internal/users"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ memberships.Repository = (*Store)(nil)

func (s *Store) CreateInvitation(ctx context.Context, nodeID string, role memberships.Role, expiresAt time.Time, maxUses int, secretDigest, creationProof [32]byte) (memberships.Invitation, error) {
	if !nodes.ValidPublicID(nodeID) {
		return memberships.Invitation{}, nodes.ErrInvalidPublicID
	}
	if err := memberships.ValidatePolicy(role, expiresAt, maxUses, time.Now()); err != nil {
		return memberships.Invitation{}, err
	}
	id, err := memberships.NewInvitationID()
	if err != nil {
		return memberships.Invitation{}, fmt.Errorf("generate invitation ID: %w", err)
	}
	invitation := memberships.Invitation{NodeID: nodeID}
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO node_invitations (public_id, node_id, secret_digest, creation_proof, role, max_uses, expires_at)
		SELECT $1, n.id, $3, $4, $5, $6, $7 FROM nodes n
		WHERE n.public_id = $2 AND n.enrollment_status = 'APPROVED' AND n.trust_status = 'TRUSTED'
		RETURNING public_id, role, max_uses, expires_at, created_at`,
		id, nodeID, secretDigest[:], creationProof[:], string(role), maxUses, expiresAt).
		Scan(&invitation.ID, &invitation.Role, &invitation.MaxUses, &invitation.ExpiresAt, &invitation.CreatedAt)
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" && pgError.ConstraintName == "node_invitations_creation_proof_key" {
		return memberships.Invitation{}, memberships.ErrReplayedCreate
	}
	if errors.Is(err, sql.ErrNoRows) {
		return memberships.Invitation{}, memberships.ErrNodeNotFound
	}
	if err != nil {
		return memberships.Invitation{}, fmt.Errorf("create invitation: %w", err)
	}
	return invitation, nil
}

func (s *Store) AcceptInvitation(ctx context.Context, secretDigest [32]byte, userID string) (memberships.Membership, error) {
	if !users.ValidUserID(userID) {
		return memberships.Membership{}, users.ErrInvalidUserID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memberships.Membership{}, err
	}
	defer tx.Rollback()
	// FOR UPDATE serializes concurrent acceptances of one invitation; the
	// WHERE clause is re-evaluated after the lock, so uses never exceed max_uses.
	var invitationID, internalNodeID int64
	var membership memberships.Membership
	err = tx.QueryRowContext(ctx,
		`SELECT i.id, i.node_id, n.public_id, i.role FROM node_invitations i JOIN nodes n ON n.id = i.node_id
		WHERE i.secret_digest = $1 AND i.expires_at > now() AND i.uses < i.max_uses
		AND n.enrollment_status = 'APPROVED' AND n.trust_status = 'TRUSTED'
		FOR UPDATE OF i`, secretDigest[:]).Scan(&invitationID, &internalNodeID, &membership.NodeID, &membership.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return memberships.Membership{}, memberships.ErrUnavailable
	}
	if err != nil {
		return memberships.Membership{}, fmt.Errorf("lock invitation: %w", err)
	}
	var internalUserID int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM global_users WHERE public_id = $1 AND status = 'ACTIVE' FOR SHARE", userID).Scan(&internalUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return memberships.Membership{}, users.ErrInactive
	}
	if err != nil {
		return memberships.Membership{}, fmt.Errorf("lock user: %w", err)
	}
	memberID, err := memberships.NewMemberID()
	if err != nil {
		return memberships.Membership{}, fmt.Errorf("generate member ID: %w", err)
	}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO node_memberships (public_id, node_id, user_id, role, invitation_id) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (node_id, user_id) DO NOTHING RETURNING public_id, created_at`,
		memberID, internalNodeID, internalUserID, string(membership.Role), invitationID).Scan(&membership.MemberID, &membership.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return memberships.Membership{}, memberships.ErrAlreadyMember
	}
	if err != nil {
		return memberships.Membership{}, fmt.Errorf("create membership: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE node_invitations SET uses = uses + 1 WHERE id = $1", invitationID); err != nil {
		return memberships.Membership{}, fmt.Errorf("consume invitation: %w", err)
	}
	return membership, tx.Commit()
}
