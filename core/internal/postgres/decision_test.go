package postgres

import (
	"context"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"arknet/core/internal/nodes"
)

var _ nodes.AdministrationRepository = (*Store)(nil)

func TestDecisionInvalidIDsDoNotAccessDatabase(t *testing.T) {
	s := &Store{}
	for _, decide := range []func(context.Context, string) (nodes.Node, error){s.Approve, s.Reject} {
		for _, id := range []string{"", "operator@example.com", " node_" + strings.Repeat("a", 64), "node_" + strings.Repeat("A", 64)} {
			if _, err := decide(context.Background(), id); err != nodes.ErrInvalidPublicID {
				t.Fatalf("error = %v", err)
			}
		}
	}
}

func TestDecisionPersistsAndPreservesMetadata(t *testing.T) {
	for _, approve := range []bool{true, false} {
		name := "reject"
		if approve {
			name = "approve"
		}
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			if err := s.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			id, err := nodes.NewPublicID()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Enroll(ctx, id, "original"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET last_seen_at=now() WHERE public_id=$1", id); err != nil {
				t.Fatal(err)
			}
			before, err := s.GetByPublicID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			decide, opposite := s.Reject, s.Approve
			want := before
			want.EnrollmentStatus = nodes.EnrollmentRejected
			want.TrustStatus = nodes.TrustBlocked
			if approve {
				decide, opposite = s.Approve, s.Reject
				want.EnrollmentStatus = nodes.EnrollmentApproved
				want.TrustStatus = nodes.TrustTrusted
			}
			got, err := decide(ctx, id)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("decision got %+v err=%v want %+v", got, err, want)
			}
			for _, action := range []func(context.Context, string) (nodes.Node, error){decide, opposite} {
				if _, err := action(ctx, id); err != nodes.ErrEnrollmentConflict {
					t.Fatalf("duplicate/opposite error=%v", err)
				}
			}
			if _, err := s.Enroll(ctx, id, "replacement"); err != nodes.ErrEnrollmentConflict {
				t.Fatalf("reenroll error=%v", err)
			}
			var schema string
			if err := s.db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
				t.Fatal(err)
			}
			dsn, err := url.Parse(os.Getenv("ARKNET_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal("parse test URL")
			}
			q := dsn.Query()
			q.Set("search_path", schema)
			dsn.RawQuery = q.Encode()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, dsn.String())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			persisted, err := reopened.GetByPublicID(ctx, id)
			if err != nil || !reflect.DeepEqual(persisted, want) {
				t.Fatalf("persisted %+v err=%v want %+v", persisted, err, want)
			}
			pending, err := reopened.ListPending(ctx, "", 100)
			if err != nil || len(pending) != 0 {
				t.Fatalf("decided node remains pending: %+v %v", pending, err)
			}
		})
	}
}

func TestDecisionMissingAndInconsistentStates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	missing, err := nodes.NewPublicID()
	if err != nil {
		t.Fatal(err)
	}
	for _, decide := range []func(context.Context, string) (nodes.Node, error){s.Approve, s.Reject} {
		if _, err := decide(ctx, missing); err != nodes.ErrNotFound {
			t.Fatalf("missing error=%v", err)
		}
	}
	for _, state := range [][2]string{{"PENDING", "TRUSTED"}, {"PENDING", "BLOCKED"}, {"APPROVED", "UNTRUSTED"}, {"REJECTED", "UNTRUSTED"}, {"OTHER", "UNTRUSTED"}, {"PENDING", "OTHER"}} {
		id, err := nodes.NewPublicID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Enroll(ctx, id, "original"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET enrollment_status=$2,trust_status=$3 WHERE public_id=$1", id, state[0], state[1]); err != nil {
			t.Fatal(err)
		}
		before, err := s.GetByPublicID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, decide := range []func(context.Context, string) (nodes.Node, error){s.Approve, s.Reject} {
			if _, err := decide(ctx, id); err != nodes.ErrEnrollmentConflict {
				t.Fatalf("inconsistent state %v error=%v", state, err)
			}
		}
		after, err := s.GetByPublicID(ctx, id)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("inconsistent state changed: %+v %v", after, err)
		}
	}
}

func TestSimultaneousMixedDecisionsExactlyOneWins(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := nodes.NewPublicID()
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Enroll(ctx, id, "original")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		node nodes.Node
		err  error
	}
	const n = 24
	results := make(chan result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			decide := s.Approve
			if i%2 != 0 {
				decide = s.Reject
			}
			node, err := decide(ctx, id)
			results <- result{node, err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	var winner nodes.Node
	for result := range results {
		if result.err == nil {
			successes++
			winner = result.node
		} else if !errors.Is(result.err, nodes.ErrEnrollmentConflict) {
			t.Fatalf("unexpected error=%v", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful decisions=%d", successes)
	}
	if !((winner.EnrollmentStatus == nodes.EnrollmentApproved && winner.TrustStatus == nodes.TrustTrusted) || (winner.EnrollmentStatus == nodes.EnrollmentRejected && winner.TrustStatus == nodes.TrustBlocked)) {
		t.Fatalf("inconsistent winner: %+v", winner)
	}
	want := before
	want.EnrollmentStatus = winner.EnrollmentStatus
	want.TrustStatus = winner.TrustStatus
	persisted, err := s.GetByPublicID(ctx, id)
	if err != nil || !reflect.DeepEqual(winner, want) || !reflect.DeepEqual(persisted, winner) {
		t.Fatalf("winner not persisted intact: %+v err=%v", persisted, err)
	}
}
