package postgres

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"arknet/node-agent/internal/localstate"
)

func historyCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM node_state_transitions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTransitionAtomicAuditAndFailures(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.StateChangedAt.IsZero() {
		t.Fatal("missing initial timestamp")
	}
	for _, next := range []localstate.State{localstate.Provisioning, localstate.Mesh, "UNKNOWN"} {
		if _, err := s.Transition(ctx, localstate.Provisioning, next); !errors.Is(err, localstate.ErrInvalidTransition) {
			t.Fatalf("invalid: %v", err)
		}
		after, err := s.Snapshot(ctx)
		if err != nil || after != before || historyCount(t, s) != 0 {
			t.Fatalf("invalid mutated state: %+v %v", after, err)
		}
	}
	after, err := s.Transition(ctx, localstate.Provisioning, localstate.EnrollmentPending)
	if err != nil || after.NodeID != before.NodeID || after.State != localstate.EnrollmentPending || !after.StateChangedAt.After(before.StateChangedAt) {
		t.Fatalf("transition: %+v %v", after, err)
	}
	var from, to string
	var at time.Time
	if err := s.db.QueryRow("SELECT from_state,to_state,transitioned_at FROM node_state_transitions").Scan(&from, &to, &at); err != nil || from != "PROVISIONING" || to != "ENROLLMENT_PENDING" || !at.Equal(after.StateChangedAt) {
		t.Fatalf("audit: %s %s %v %v", from, to, at, err)
	}
	if _, err := s.Transition(ctx, localstate.Provisioning, localstate.Error); !errors.Is(err, localstate.ErrStateConflict) {
		t.Fatalf("stale: %v", err)
	}
	current, err := s.Snapshot(ctx)
	if err != nil || current != after || historyCount(t, s) != 1 {
		t.Fatal("stale transition mutated")
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	current, err = s.Snapshot(ctx)
	if err != nil || current != after {
		t.Fatal("initialize reset state timestamp")
	}
	if _, err := s.Transition(ctx, "UNKNOWN", localstate.Connected); !errors.Is(err, localstate.ErrInvalidTransition) {
		t.Fatalf("unknown expected: %v", err)
	}
	current, err = s.Snapshot(ctx)
	if err != nil || current != after || historyCount(t, s) != 1 {
		t.Fatal("unknown expected mutated state")
	}
	// Force the audit INSERT to fail after UPDATE; the entire transaction must roll back.
	if _, err := s.db.Exec("ALTER TABLE node_state_transitions ADD CONSTRAINT reject_audit CHECK (to_state <> 'CONNECTED')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(ctx, localstate.EnrollmentPending, localstate.Connected); err == nil {
		t.Fatal("expected audit failure")
	}
	current, err = s.Snapshot(ctx)
	if err != nil || current != after || historyCount(t, s) != 1 {
		t.Fatal("audit failure did not roll back")
	}
}

func TestConcurrentTransitionsCAS(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Transition(ctx, localstate.Provisioning, localstate.EnrollmentPending)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, localstate.ErrStateConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 15 || historyCount(t, s) != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}

func TestV1UpgradeAndUnknownStateRollback(t *testing.T) {
	for _, state := range []string{"CONNECTED", "CUSTOM_STATE"} {
		t.Run(state, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			data, err := migrationFiles.ReadFile("migrations/0001_local_state.sql")
			if err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := migrate(ctx, tx, fstest.MapFS{"migrations/0001_local_state.sql": {Data: data}}); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("INSERT INTO node_identity(singleton,public_id) VALUES(true,'node_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'); INSERT INTO node_configuration VALUES(true,'{}'); INSERT INTO core_enrollment VALUES(true,'NOT_ENROLLED',NULL,NULL)"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("INSERT INTO node_state(singleton,state) VALUES(true,$1)", state); err != nil {
				t.Fatal(err)
			}
			err = s.Initialize(ctx)
			if state == "CUSTOM_STATE" {
				if err == nil {
					t.Fatal("unknown v1 state accepted")
				}
				var stored string
				var count int
				var history sql.NullString
				if err := s.db.QueryRow("SELECT state FROM node_state").Scan(&stored); err != nil || stored != state {
					t.Fatal("state normalized")
				}
				if err := s.db.QueryRow("SELECT count(*) FROM arknet_schema_migrations").Scan(&count); err != nil || count != 1 {
					t.Fatal("migration ledger mutated")
				}
				if err := s.db.QueryRow("SELECT to_regclass('node_state_transitions')::text").Scan(&history); err != nil || history.Valid {
					t.Fatal("migration history DDL did not roll back")
				}
				var changedAtColumns int
				if err := s.db.QueryRow("SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='node_state' AND column_name='changed_at'").Scan(&changedAtColumns); err != nil || changedAtColumns != 0 {
					t.Fatal("migration column did not roll back")
				}
				if err := s.Initialize(ctx); err == nil {
					t.Fatal("unknown replay accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.Snapshot(ctx)
			if err != nil || before.State != localstate.Connected || before.StateChangedAt.IsZero() {
				t.Fatalf("upgrade: %+v %v", before, err)
			}
			if err := s.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			after, err := s.Snapshot(ctx)
			if err != nil || after != before || historyCount(t, s) != 0 {
				t.Fatal("upgrade replay changed data")
			}
		})
	}
}
