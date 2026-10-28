package postgres

import (
	"arknet/node-agent/internal/localstate"
	"bytes"
	"context"
	"database/sql"
	"sync"
	"testing"
)

func TestCredentialConcurrentInitializeAndRestart(t *testing.T) {
	s := initializedEnrollmentStore(t)
	ctx := context.Background()
	first, err := s.EnrollmentIdentity(ctx)
	if err != nil || len(first.PrivateSeed) != 32 {
		t.Fatalf("identity %v", err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Initialize(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	second, err := s.EnrollmentIdentity(ctx)
	if err != nil || first.NodeID != second.NodeID || !bytes.Equal(first.PrivateSeed, second.PrivateSeed) {
		t.Fatal("credential rotated")
	}
	other := initializedEnrollmentStore(t)
	different, _ := other.EnrollmentIdentity(ctx)
	if bytes.Equal(first.PrivateSeed, different.PrivateSeed) {
		t.Fatal("shared credentials")
	}
}
func TestCompleteEnrollmentAtomicIdempotentAndFailClosed(t *testing.T) {
	s := initializedEnrollmentStore(t)
	ctx := context.Background()
	id, _ := s.EnrollmentIdentity(ctx)
	if _, err := s.RecordEnrollmentPending(ctx, "https://core.example"); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"https://wrong.example", id.NodeID}, {"https://core.example", "node_bad"}} {
		if _, err := s.CompleteEnrollment(ctx, pair[0], pair[1]); err == nil {
			t.Fatal("binding mismatch accepted")
		}
	}
	before, _ := s.Snapshot(ctx)
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE core_enrollment ADD CONSTRAINT refuse_approval CHECK(status <> 'APPROVED')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteEnrollment(ctx, "https://core.example", id.NodeID); err == nil {
		t.Fatal("expected rollback")
	}
	after, _ := s.Snapshot(ctx)
	if after != before {
		t.Fatal("partial completion")
	}
	s.db.ExecContext(ctx, "ALTER TABLE core_enrollment DROP CONSTRAINT refuse_approval")
	completed, err := s.CompleteEnrollment(ctx, "https://core.example", id.NodeID)
	if err != nil || completed.State != localstate.Connected {
		t.Fatalf("completion %v %v", completed, err)
	}
	var first, second sql.NullTime
	s.db.QueryRowContext(ctx, "SELECT enrolled_at FROM core_enrollment").Scan(&first)
	again, err := s.CompleteEnrollment(ctx, "https://core.example", id.NodeID)
	s.db.QueryRowContext(ctx, "SELECT enrolled_at FROM core_enrollment").Scan(&second)
	if err != nil || again != completed || first != second || !first.Valid {
		t.Fatal("duplicate ack changed timestamps")
	}
	s.db.ExecContext(ctx, "DELETE FROM node_federation_credential")
	if s.Initialize(ctx) == nil {
		t.Fatal("completed missing credential regenerated")
	}
	if _, err := s.EnrollmentIdentity(ctx); err == nil {
		t.Fatal("missing completed credential accepted")
	}
}
func TestCompleteEnrollmentCASDoesNotResetOtherState(t *testing.T) {
	s := initializedEnrollmentStore(t)
	ctx := context.Background()
	id, _ := s.EnrollmentIdentity(ctx)
	s.RecordEnrollmentPending(ctx, "https://core.example")
	changed, err := s.Transition(ctx, localstate.EnrollmentPending, localstate.Error)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteEnrollment(ctx, "https://core.example", id.NodeID); err == nil {
		t.Fatal("late ack accepted")
	}
	after, _ := s.Snapshot(ctx)
	if after != changed {
		t.Fatal("late ack reset state")
	}
}
