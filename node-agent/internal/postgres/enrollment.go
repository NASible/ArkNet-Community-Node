package postgres

import (
	"arknet/node-agent/internal/localstate"
	"context"
	"database/sql"
	"errors"
	"time"
)

// RecordEnrollmentPending atomically records a validated Core acknowledgement.
// Lock order matches Transition's state-row lock, so a concurrent local state
// change cannot be overwritten. Repeated pending acknowledgements are no-ops
// for the state timestamp and transition history.
func (s *Store) RecordEnrollmentPending(ctx context.Context, coreURL string) (localstate.Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return localstate.Snapshot{}, err
	}
	defer tx.Rollback()
	var snapshot localstate.Snapshot
	if err := tx.QueryRowContext(ctx, `SELECT i.public_id,s.state,s.changed_at
 FROM node_identity i CROSS JOIN node_state s
 WHERE i.singleton=true AND s.singleton=true FOR UPDATE OF s`).Scan(&snapshot.NodeID, &snapshot.State, &snapshot.StateChangedAt); err != nil {
		return localstate.Snapshot{}, err
	}
	if !validNodeID(snapshot.NodeID) {
		return localstate.Snapshot{}, errors.New("invalid stored node identity")
	}
	if snapshot.State != localstate.Provisioning && snapshot.State != localstate.EnrollmentPending {
		return localstate.Snapshot{}, localstate.ErrStateConflict
	}
	var status string
	var boundURL sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT status,core_url FROM core_enrollment WHERE singleton=true FOR UPDATE").Scan(&status, &boundURL); err != nil {
		return localstate.Snapshot{}, err
	}
	if coreURL == "" || (boundURL.Valid && boundURL.String != coreURL) || (status != "NOT_ENROLLED" && status != "PENDING") {
		return localstate.Snapshot{}, errors.New("enrollment conflict")
	}
	if snapshot.State == localstate.Provisioning {
		if err := localstate.Validate(snapshot.State, localstate.EnrollmentPending); err != nil {
			return localstate.Snapshot{}, err
		}
		if err := tx.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&snapshot.StateChangedAt); err != nil {
			return localstate.Snapshot{}, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE node_state SET state=$1,changed_at=$2 WHERE singleton=true", localstate.EnrollmentPending, snapshot.StateChangedAt); err != nil {
			return localstate.Snapshot{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO node_state_transitions(from_state,to_state,transitioned_at) VALUES($1,$2,$3)", snapshot.State, localstate.EnrollmentPending, snapshot.StateChangedAt); err != nil {
			return localstate.Snapshot{}, err
		}
		snapshot.State = localstate.EnrollmentPending
	}
	if _, err := tx.ExecContext(ctx, "UPDATE core_enrollment SET status='PENDING',core_url=$1,enrolled_at=NULL WHERE singleton=true", coreURL); err != nil {
		return localstate.Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return localstate.Snapshot{}, err
	}
	snapshot.EnrollmentState = "PENDING"
	snapshot.LastSuccessfulCoreContactAt = time.Time{}
	return snapshot, nil
}
