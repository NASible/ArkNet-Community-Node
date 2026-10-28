package postgres

import (
	"context"
	"errors"
	"testing"

	"arknet/node-agent/internal/localstate"
)

func TestDatabaseTransitionsAllPairs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	states := []localstate.State{localstate.Provisioning, localstate.EnrollmentPending, localstate.Connected, localstate.Synchronizing, localstate.Mesh, localstate.Degraded, localstate.Offline, localstate.Reconnecting, localstate.Quarantined, localstate.Revoked, localstate.Error}
	for _, from := range states {
		for _, to := range states {
			t.Run(string(from)+"/"+string(to), func(t *testing.T) {
				if _, err := s.db.Exec("UPDATE node_state SET state=$1", from); err != nil {
					t.Fatal(err)
				}
				before, err := s.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				count := historyCount(t, s)
				result, err := s.Transition(ctx, from, to)
				if localstate.Validate(from, to) != nil {
					if !errors.Is(err, localstate.ErrInvalidTransition) {
						t.Fatalf("invalid: %v", err)
					}
					after, err := s.Snapshot(ctx)
					if err != nil || after != before || historyCount(t, s) != count {
						t.Fatal("invalid mutated state or history")
					}
					return
				}
				if err != nil || result.State != to || !result.StateChangedAt.After(before.StateChangedAt) || historyCount(t, s) != count+1 {
					t.Fatalf("valid: %+v %v", result, err)
				}
				after, err := s.Snapshot(ctx)
				if err != nil || after != result {
					t.Fatal("returned snapshot differs from persisted state")
				}
			})
		}
	}
}

func TestStateSchemaConstraints(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"UPDATE node_state SET state='CUSTOM_STATE'",
		"UPDATE node_state SET changed_at=NULL",
		"INSERT INTO node_state_transitions(from_state,to_state,transitioned_at) VALUES('UNKNOWN','CONNECTED',clock_timestamp())",
		"INSERT INTO node_state_transitions(from_state,to_state,transitioned_at) VALUES('CONNECTED','UNKNOWN',clock_timestamp())",
		"INSERT INTO node_state_transitions(from_state,to_state,transitioned_at) VALUES('CONNECTED','CONNECTED',clock_timestamp())",
		"INSERT INTO node_state_transitions(from_state,to_state,transitioned_at) VALUES('CONNECTED','SYNCHRONIZING',NULL)",
	} {
		if _, err := s.db.Exec(query); err == nil {
			t.Fatalf("constraint accepted %s", query)
		}
	}
	if historyCount(t, s) != 0 {
		t.Fatal("invalid audit inserted")
	}
}

func TestStoredUnknownStateRejectedWithoutRepair(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("ALTER TABLE node_state DROP CONSTRAINT node_state_valid; UPDATE node_state SET state='CUSTOM_STATE'; DELETE FROM node_configuration"); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx); !errors.Is(err, localstate.ErrInvalidTransition) {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := s.Snapshot(ctx); !errors.Is(err, localstate.ErrInvalidTransition) {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := s.Transition(ctx, localstate.Provisioning, localstate.EnrollmentPending); !errors.Is(err, localstate.ErrInvalidTransition) {
		t.Fatalf("transition: %v", err)
	}
	var state string
	var count int
	if err := s.db.QueryRow("SELECT state FROM node_state").Scan(&state); err != nil || state != "CUSTOM_STATE" {
		t.Fatal("unknown state repaired")
	}
	if err := s.db.QueryRow("SELECT count(*) FROM node_configuration").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed initialize restored defaults")
	}
	if historyCount(t, s) != 0 {
		t.Fatal("unknown state audited")
	}
}
