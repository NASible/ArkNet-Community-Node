package postgres

import (
	"arknet/node-agent/internal/localstate"
	"context"
	"sync"
	"testing"
)

func TestConcurrentCompletionAddsExactlyOneApprovalTransition(t *testing.T) {
	s := initializedEnrollmentStore(t)
	ctx := context.Background()
	id, err := s.EnrollmentIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEnrollmentPending(ctx, "https://core.example"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan localstate.Snapshot, 12)
	failures := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := s.CompleteEnrollment(ctx, "https://core.example", id.NodeID)
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
	final, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for result := range results {
		if result != final {
			t.Fatal("concurrent approval changed timestamp")
		}
	}
	if historyCount(t, s) != 2 {
		t.Fatal("approval transition duplicated")
	}
}
func TestCredentialImmutableAndBoundToStoredID(t *testing.T) {
	s := initializedEnrollmentStore(t)
	ctx := context.Background()
	id, err := s.EnrollmentIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE node_federation_credential SET private_seed=$1", make([]byte, 32)); err == nil {
		t.Fatal("credential mutable")
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE node_identity SET public_id=$1", "node_"+stringRepeatOther(id.NodeID[5:])); err != nil {
		t.Fatal(err)
	}
	if s.Initialize(ctx) == nil {
		t.Fatal("credential rebound to different stored identity")
	}
	if _, err := s.EnrollmentIdentity(ctx); err == nil {
		t.Fatal("mismatched identity returned")
	}
}
func stringRepeatOther(s string) string {
	b := []byte(s)
	if b[0] == 'a' {
		b[0] = 'b'
	} else {
		b[0] = 'a'
	}
	return string(b)
}
