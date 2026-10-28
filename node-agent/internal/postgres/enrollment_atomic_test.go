package postgres

import (
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func initializedEnrollmentStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	if err := s.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}
func assertUnenrolled(t *testing.T, s *Store, before localstate.Snapshot) {
	t.Helper()
	ctx := context.Background()
	after, err := s.Snapshot(ctx)
	if err != nil || after != before {
		t.Fatalf("state changed: %+v %v", after, err)
	}
	var status string
	var url sql.NullString
	var enrolled sql.NullTime
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT status,core_url,enrolled_at FROM core_enrollment").Scan(&status, &url, &enrolled); err != nil || status != "NOT_ENROLLED" || url.Valid || enrolled.Valid {
		t.Fatalf("enrollment mutated %s %v %v %v", status, url, enrolled, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM node_state_transitions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("history changed %d %v", count, err)
	}
}
func TestEnrollmentInvalidAckLeavesDatabaseUnchanged(t *testing.T) {
	s := initializedEnrollmentStore(t)
	ctx := context.Background()
	before, _ := s.Snapshot(ctx)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"node_id":"wrong","enrollment_status":"PENDING"}`))
	}))
	defer server.Close()
	if err := enrollment.Submit(ctx, server.URL, "test", s); err == nil {
		t.Fatal("invalid ack accepted")
	}
	assertUnenrolled(t, s, before)
}
func TestEnrollmentApprovedStatusCannotBeReset(t *testing.T) {
	for _, status := range []string{"APPROVED", "REVOKED", "UNKNOWN"} {
		t.Run(status, func(t *testing.T) {
			s := initializedEnrollmentStore(t)
			ctx := context.Background()
			if _, err := s.db.ExecContext(ctx, "UPDATE core_enrollment SET status=$1,core_url='https://core.example',enrolled_at=clock_timestamp()", status); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Snapshot(ctx)
			var oldTime, newTime sql.NullTime
			s.db.QueryRowContext(ctx, "SELECT enrolled_at FROM core_enrollment").Scan(&oldTime)
			if _, err := s.RecordEnrollmentPending(ctx, "https://core.example"); err == nil {
				t.Fatal("approved enrollment reset")
			}
			after, _ := s.Snapshot(ctx)
			if after != before {
				t.Fatal("state advanced despite enrollment conflict")
			}
			var actual string
			s.db.QueryRowContext(ctx, "SELECT status,enrolled_at FROM core_enrollment").Scan(&actual, &newTime)
			if actual != status || newTime != oldTime {
				t.Fatal("enrollment changed")
			}
		})
	}
}
func TestEnrollmentDisallowedStateCannotAdvance(t *testing.T) {
	for _, state := range []localstate.State{localstate.Connected, localstate.Synchronizing, localstate.Mesh, localstate.Degraded, localstate.Offline, localstate.Reconnecting, localstate.Quarantined, localstate.Revoked, localstate.Error} {
		t.Run(string(state), func(t *testing.T) {
			s := initializedEnrollmentStore(t)
			ctx := context.Background()
			if _, err := s.db.ExecContext(ctx, "UPDATE node_state SET state=$1", state); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Snapshot(ctx)
			if _, err := s.RecordEnrollmentPending(ctx, "https://core.example"); err == nil {
				t.Fatal("disallowed state accepted")
			}
			assertUnenrolled(t, s, before)
		})
	}
}
func TestEnrollmentFailureRollsBackStateAndHistory(t *testing.T) {
	s := initializedEnrollmentStore(t)
	ctx := context.Background()
	before, _ := s.Snapshot(ctx)
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE core_enrollment ADD CONSTRAINT refuse_pending CHECK(status <> 'PENDING')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEnrollmentPending(ctx, "https://core.example"); err == nil {
		t.Fatal("expected storage failure")
	}
	assertUnenrolled(t, s, before)
}
func TestEnrollmentConcurrentPendingOnlyOneTransition(t *testing.T) {
	s := initializedEnrollmentStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan localstate.Snapshot, 12)
	failures := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := s.RecordEnrollmentPending(ctx, "https://core.example")
			results <- snap
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	final, _ := s.Snapshot(ctx)
	for snapshot := range results {
		if snapshot != final {
			t.Fatal("concurrent timestamp changed")
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM node_state_transitions").Scan(&count); err != nil || count != 1 {
		t.Fatalf("history %d %v", count, err)
	}
}
func TestEnrollmentAckRacingStateChangeCannotOverwrite(t *testing.T) {
	s := initializedEnrollmentStore(t)
	ctx := context.Background()
	initial, _ := s.Snapshot(ctx)
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		json.NewEncoder(w).Encode(map[string]string{"node_id": initial.NodeID, "enrollment_status": "PENDING"})
	}))
	defer server.Close()
	result := make(chan error, 1)
	go func() { result <- enrollment.Submit(ctx, server.URL, "test", s) }()
	<-entered
	changed, err := s.Transition(ctx, localstate.Provisioning, localstate.Error)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("late ack overwrote local state")
	}
	final, _ := s.Snapshot(ctx)
	if final != changed {
		t.Fatal("late ack changed state")
	}
	var status string
	var url sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT status,core_url FROM core_enrollment").Scan(&status, &url); err != nil || status != "NOT_ENROLLED" || url.Valid {
		t.Fatal("race changed enrollment")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM node_state_transitions").Scan(&count); err != nil || count != 1 {
		t.Fatal("race added audit record")
	}
}
func TestEnrollmentLockWaitCancellationDoesNotMutate(t *testing.T) {
	s := initializedEnrollmentStore(t)
	ctx := context.Background()
	before, _ := s.Snapshot(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT state FROM node_state FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.RecordEnrollmentPending(cancelCtx, "https://core.example"); err == nil {
		t.Fatal("cancelled operation accepted")
	}
	tx.Rollback()
	assertUnenrolled(t, s, before)
}
