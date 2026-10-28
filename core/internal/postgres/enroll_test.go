package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"arknet/core/internal/nodes"
)

func TestEnrollInvalidInputsDoNotAccessDatabase(t *testing.T) {
	s := &Store{}
	if _, err := s.Enroll(context.Background(), "operator@example.com", "v"); !errors.Is(err, nodes.ErrInvalidPublicID) {
		t.Fatalf("error = %v", err)
	}
	for _, version := range []string{"", "  ", strings.Repeat("v", 65), "v\n", "é"} {
		if _, err := s.Enroll(context.Background(), "node_"+strings.Repeat("a", 64), version); !errors.Is(err, nodes.ErrInvalidSoftwareVersion) {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestEnrollRepeatPreservesExistingRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := nodes.NewPublicID()
	first, err := s.Enroll(ctx, id, "original")
	if err != nil {
		t.Fatal(err)
	}
	if first.PublicID != id || first.ID <= 0 || first.CreatedAt.IsZero() || first.SoftwareVersion != "original" || first.EnrollmentStatus != nodes.EnrollmentPending || first.TrustStatus != nodes.TrustUntrusted || first.LastSeenAt != nil {
		t.Fatalf("unsafe initial record: %+v", first)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET last_seen_at = now() WHERE public_id=$1", id); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetByPublicID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Enroll(ctx, id, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.GetByPublicID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, again) || !reflect.DeepEqual(before, after) {
		t.Fatal("retry changed existing record")
	}
}

func TestEnrollConcurrentRetriesCreateExactlyOneNode(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := nodes.NewPublicID()
	const n = 24
	results := make(chan nodes.Node, n)
	errs := make(chan error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			node, err := s.Enroll(ctx, id, fmt.Sprintf("v%d", i))
			results <- node
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first nodes.Node
	for node := range results {
		if first.ID == 0 {
			first = node
		}
		if !reflect.DeepEqual(first, node) {
			t.Fatal("concurrent retry did not return original node")
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM nodes WHERE public_id=$1", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d", count)
	}
}

func TestEnrollConflictsPreserveExistingStatus(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, state := range [][2]string{{"APPROVED", "UNTRUSTED"}, {"PENDING", "TRUSTED"}, {"REJECTED", "BLOCKED"}} {
		id, _ := nodes.NewPublicID()
		if _, err := s.Enroll(ctx, id, "original"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET enrollment_status=$2, trust_status=$3, last_seen_at=now() WHERE public_id=$1", id, state[0], state[1]); err != nil {
			t.Fatal(err)
		}
		before, err := s.GetByPublicID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Enroll(ctx, id, "replacement"); !errors.Is(err, nodes.ErrEnrollmentConflict) {
			t.Fatalf("error = %v", err)
		}
		after, err := s.GetByPublicID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("conflict changed record")
		}
	}
}
