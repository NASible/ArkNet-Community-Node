package postgres

import (
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
)

func ensureCredential(ctx context.Context, tx *sql.Tx, id string) error {
	var status, storedID string
	var seed []byte
	if err := tx.QueryRowContext(ctx, "SELECT status FROM core_enrollment WHERE singleton=true FOR UPDATE").Scan(&status); err != nil {
		return err
	}
	err := tx.QueryRowContext(ctx, "SELECT node_public_id,private_seed FROM node_federation_credential WHERE singleton=true").Scan(&storedID, &seed)
	if errors.Is(err, sql.ErrNoRows) {
		if status != "NOT_ENROLLED" && status != "PENDING" {
			return errors.New("missing federation credential")
		}
		seed = make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			return errors.New("credential generation failed")
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO node_federation_credential(singleton,node_public_id,private_seed) VALUES(true,$1,$2)", id, seed)
		return err
	}
	if err != nil {
		return err
	}
	if storedID != id || len(seed) != 32 {
		return errors.New("invalid federation credential")
	}
	return nil
}

// EnrollmentIdentity returns private internal material; it must never reach HTTP handlers or logs.
func (s *Store) EnrollmentIdentity(ctx context.Context) (enrollment.Identity, error) {
	var id enrollment.Identity
	var bound sql.NullString
	var storedID string
	err := s.db.QueryRowContext(ctx, `SELECT i.public_id,e.core_url,e.status,c.node_public_id,c.private_seed
 FROM node_identity i CROSS JOIN core_enrollment e CROSS JOIN node_federation_credential c
 WHERE i.singleton=true AND e.singleton=true AND c.singleton=true`).Scan(&id.NodeID, &bound, &id.Status, &storedID, &id.PrivateSeed)
	if err != nil || !validNodeID(id.NodeID) || storedID != id.NodeID || len(id.PrivateSeed) != 32 {
		return enrollment.Identity{}, errors.New("invalid federation identity")
	}
	id.CoreURL = bound.String
	return id, nil
}

// CompleteEnrollment commits approval and the pending-to-connected transition together.
func (s *Store) CompleteEnrollment(ctx context.Context, coreURL, nodeID string) (localstate.Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return localstate.Snapshot{}, err
	}
	defer tx.Rollback()
	var snap localstate.Snapshot
	if err := tx.QueryRowContext(ctx, `SELECT i.public_id,s.state,s.changed_at FROM node_identity i CROSS JOIN node_state s WHERE i.singleton=true AND s.singleton=true FOR UPDATE OF s`).Scan(&snap.NodeID, &snap.State, &snap.StateChangedAt); err != nil {
		return localstate.Snapshot{}, err
	}
	var status string
	var bound sql.NullString
	var lastContact sql.NullTime
	if err := tx.QueryRowContext(ctx, "SELECT status,core_url,last_successful_core_contact_at FROM core_enrollment WHERE singleton=true FOR UPDATE").Scan(&status, &bound, &lastContact); err != nil {
		return localstate.Snapshot{}, err
	}
	snap.EnrollmentState = status
	if lastContact.Valid {
		snap.LastSuccessfulCoreContactAt = lastContact.Time
	}
	var credentialID string
	if err := tx.QueryRowContext(ctx, "SELECT node_public_id FROM node_federation_credential WHERE singleton=true").Scan(&credentialID); err != nil {
		return localstate.Snapshot{}, err
	}
	if !validNodeID(nodeID) || nodeID != snap.NodeID || credentialID != nodeID || coreURL == "" || !bound.Valid || bound.String != coreURL {
		return localstate.Snapshot{}, errors.New("enrollment conflict")
	}
	if status == "APPROVED" {
		return snap, tx.Commit()
	}
	if status != "PENDING" || snap.State != localstate.EnrollmentPending {
		return localstate.Snapshot{}, localstate.ErrStateConflict
	}
	if err := tx.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&snap.StateChangedAt); err != nil {
		return localstate.Snapshot{}, err
	}
	for _, q := range []string{"UPDATE core_enrollment SET status='APPROVED',enrolled_at=$1 WHERE singleton=true", "UPDATE node_state SET state='CONNECTED',changed_at=$1 WHERE singleton=true", "INSERT INTO node_state_transitions(from_state,to_state,transitioned_at) VALUES('ENROLLMENT_PENDING','CONNECTED',$1)"} {
		if _, err := tx.ExecContext(ctx, q, snap.StateChangedAt); err != nil {
			return localstate.Snapshot{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return localstate.Snapshot{}, err
	}
	snap.State = localstate.Connected
	snap.EnrollmentState = "APPROVED"
	return snap, nil
}
