// Package postgres implements the Node Agent's local PostgreSQL storage.
package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"arknet/node-agent/internal/localstate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type Store struct{ db *sql.DB }

var _ localstate.Repository = (*Store)(nil)
var _ localstate.StateMachine = (*Store)(nil)

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	cfg.ConnectTimeout = 5 * time.Second
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("database connection failed; check PostgreSQL and connection configuration")
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Initialize serializes startup and atomically migrates and restores missing
// singleton rows. Existing configuration, state and enrollment are never reset.
func (s *Store) Initialize(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('arknet-node-agent-local-state'))"); err != nil {
		return err
	}
	if err := migrate(ctx, tx, migrationFiles); err != nil {
		return err
	}
	var id string
	err = tx.QueryRowContext(ctx, "SELECT public_id FROM node_identity WHERE singleton = true").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Missing identity alongside persisted local data is ambiguous corruption,
		// not a fresh install. Do not invent a replacement identity.
		var existing bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM node_configuration)
   OR EXISTS (SELECT 1 FROM node_state) OR EXISTS (SELECT 1 FROM core_enrollment)`).Scan(&existing); err != nil {
			return err
		}
		if existing {
			return errors.New("missing node identity with existing local state")
		}
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return fmt.Errorf("generate node identity: %w", err)
		}
		id = "node_" + hex.EncodeToString(random)
		if _, err := tx.ExecContext(ctx, "INSERT INTO node_identity (singleton, public_id) VALUES (true, $1)", id); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if !validNodeID(id) {
		return errors.New("invalid stored node identity")
	}
	for _, statement := range []string{
		"INSERT INTO node_configuration (singleton, settings) VALUES (true, '{}'::jsonb) ON CONFLICT (singleton) DO NOTHING",
		"INSERT INTO node_state (singleton, state) VALUES (true, 'PROVISIONING') ON CONFLICT (singleton) DO NOTHING",
		"INSERT INTO core_enrollment (singleton, status, core_url, enrolled_at) VALUES (true, 'NOT_ENROLLED', NULL, NULL) ON CONFLICT (singleton) DO NOTHING",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	var state localstate.State
	if err := tx.QueryRowContext(ctx, "SELECT state FROM node_state WHERE singleton = true").Scan(&state); err != nil {
		return err
	}
	if !state.Valid() {
		return fmt.Errorf("%w: unknown stored state %q", localstate.ErrInvalidTransition, state)
	}
	if err := ensureCredential(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Snapshot(ctx context.Context) (localstate.Snapshot, error) {
	var snapshot localstate.Snapshot
	var lastContact sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT i.public_id, s.state, s.changed_at,
 e.status, e.last_successful_core_contact_at
 FROM node_identity i CROSS JOIN node_state s CROSS JOIN core_enrollment e
 WHERE i.singleton = true AND s.singleton = true AND e.singleton = true`).Scan(
		&snapshot.NodeID, &snapshot.State, &snapshot.StateChangedAt,
		&snapshot.EnrollmentState, &lastContact,
	)
	if err != nil {
		return localstate.Snapshot{}, fmt.Errorf("read local state: %w", err)
	}
	if !validNodeID(snapshot.NodeID) {
		return localstate.Snapshot{}, errors.New("invalid stored node identity")
	}
	if !snapshot.State.Valid() {
		return localstate.Snapshot{}, fmt.Errorf("%w: unknown stored state %q", localstate.ErrInvalidTransition, snapshot.State)
	}
	if lastContact.Valid {
		snapshot.LastSuccessfulCoreContactAt = lastContact.Time
	}
	return snapshot, nil
}

// Transition serializes state changes using the singleton row lock. Both the
// timestamp and audit row commit with the state or are rolled back together.
func (s *Store) Transition(ctx context.Context, expected, next localstate.State) (localstate.Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return localstate.Snapshot{}, err
	}
	defer tx.Rollback()
	var snapshot localstate.Snapshot
	var lastContact sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT i.public_id, s.state, s.changed_at,
 e.status, e.last_successful_core_contact_at
 FROM node_identity i CROSS JOIN node_state s CROSS JOIN core_enrollment e
 WHERE i.singleton = true AND s.singleton = true AND e.singleton = true
 FOR UPDATE OF s`).Scan(&snapshot.NodeID, &snapshot.State, &snapshot.StateChangedAt, &snapshot.EnrollmentState, &lastContact)
	if err != nil {
		return localstate.Snapshot{}, fmt.Errorf("lock local state: %w", err)
	}
	if !validNodeID(snapshot.NodeID) {
		return localstate.Snapshot{}, errors.New("invalid stored node identity")
	}
	if !expected.Valid() || !snapshot.State.Valid() {
		return localstate.Snapshot{}, fmt.Errorf("%w: unknown expected or stored state", localstate.ErrInvalidTransition)
	}
	if snapshot.State != expected {
		return localstate.Snapshot{}, fmt.Errorf("%w: expected %q, stored %q", localstate.ErrStateConflict, expected, snapshot.State)
	}
	if err := localstate.Validate(snapshot.State, next); err != nil {
		return localstate.Snapshot{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&snapshot.StateChangedAt); err != nil {
		return localstate.Snapshot{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE node_state SET state = $1, changed_at = $2 WHERE singleton = true", next, snapshot.StateChangedAt); err != nil {
		return localstate.Snapshot{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO node_state_transitions (from_state, to_state, transitioned_at) VALUES ($1, $2, $3)", snapshot.State, next, snapshot.StateChangedAt); err != nil {
		return localstate.Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return localstate.Snapshot{}, err
	}
	snapshot.State = next
	if lastContact.Valid {
		snapshot.LastSuccessfulCoreContactAt = lastContact.Time
	}
	return snapshot, nil
}

func validNodeID(id string) bool {
	if len(id) != 69 || id[:5] != "node_" {
		return false
	}
	for _, ch := range id[5:] {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}
