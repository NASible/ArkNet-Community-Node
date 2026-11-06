package postgres

import (
	"arknet/core/internal/nodes"
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

var _ nodes.HandshakeRepository = (*Store)(nil)

func scanIdentity(row scanner) (nodes.Node, []byte, error) {
	var node nodes.Node
	var key, health []byte
	var capabilities string
	err := row.Scan(&node.ID, &node.PublicID, &node.EnrollmentStatus, &node.TrustStatus, &node.SoftwareVersion, &node.CreatedAt, &node.LastSeenAt, &node.CurrentState, &health, &capabilities, &key)
	if err == nil {
		err = decodeHealth(&node, health)
	}
	if err == nil {
		err = decodeCapabilities(&node, capabilities)
	}
	return node, key, err
}
func handshakeError(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return nodes.ErrEnrollmentConflict
	}
	return err
}

// The row lock serializes first binding with administration's conditional
// UPDATE. Unique key ownership is enforced across nodes by PostgreSQL, including
// concurrent first registrations. A failed binding rolls back any new row.
func (s *Store) RegisterEnrollment(ctx context.Context, id, version string, key []byte) (nodes.Node, error) {
	if !nodes.ValidPublicID(id) {
		return nodes.Node{}, nodes.ErrInvalidPublicID
	}
	if !nodes.ValidSoftwareVersion(version) {
		return nodes.Node{}, nodes.ErrInvalidSoftwareVersion
	}
	if len(key) != ed25519.PublicKeySize {
		return nodes.Node{}, nodes.ErrEnrollmentConflict
	}
	if s == nil || s.db == nil {
		return nodes.Node{}, errors.New("enrollment database unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nodes.Node{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "INSERT INTO nodes (public_id,enrollment_status,trust_status,software_version) VALUES ($1,$2,$3,$4) ON CONFLICT (public_id) DO NOTHING", id, nodes.EnrollmentPending, nodes.TrustUntrusted, version)
	if err != nil {
		return nodes.Node{}, handshakeError(err)
	}
	node, stored, err := scanIdentity(tx.QueryRowContext(ctx, "SELECT "+columns+", enrollment_public_key FROM nodes WHERE public_id=$1 FOR UPDATE", id))
	if err != nil {
		return nodes.Node{}, err
	}
	if node.EnrollmentStatus != nodes.EnrollmentPending || node.TrustStatus != nodes.TrustUntrusted {
		return nodes.Node{}, nodes.ErrEnrollmentConflict
	}
	if stored != nil && !bytes.Equal(stored, key) {
		return nodes.Node{}, nodes.ErrEnrollmentConflict
	}
	if stored == nil {
		_, err = tx.ExecContext(ctx, "UPDATE nodes SET enrollment_public_key=$2 WHERE public_id=$1", id, key)
		if err != nil {
			return nodes.Node{}, handshakeError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nodes.Node{}, handshakeError(err)
	}
	return node, nil
}
func (s *Store) EnrollmentIdentity(ctx context.Context, id string) (nodes.Node, []byte, error) {
	if !nodes.ValidPublicID(id) {
		return nodes.Node{}, nil, nodes.ErrInvalidPublicID
	}
	if s == nil || s.db == nil {
		return nodes.Node{}, nil, errors.New("enrollment database unavailable")
	}
	node, key, err := scanIdentity(s.db.QueryRowContext(ctx, "SELECT "+columns+", enrollment_public_key FROM nodes WHERE public_id=$1", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nodes.Node{}, nil, nodes.ErrNotFound
	}
	return node, key, err
}
