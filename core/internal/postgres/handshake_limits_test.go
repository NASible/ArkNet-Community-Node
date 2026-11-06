package postgres

import (
	"arknet/core/internal/nodes"
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestHandshakePendingQuotaConcurrent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Existing unsigned rows count toward the same global pending limit.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO nodes(public_id,software_version,created_at) SELECT 'node_' || lpad(to_hex(i),64,'0'), 'seed', CASE WHEN i=1000 THEN now()-interval '7 days' ELSE now() END FROM generate_series(1,1000) i`); err != nil {
		t.Fatal(err)
	}
	const attempts = 24
	start := make(chan struct{})
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.RegisterEnrollment(ctx, fmt.Sprintf("node_%064x", 1001+i), "new", bytes.Repeat([]byte{byte(i + 1)}, 32))
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, nodes.ErrEnrollmentConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != attempts-1 {
		t.Fatalf("quota oversubscribed: wins=%d conflicts=%d", wins, conflicts)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM nodes WHERE enrollment_status='PENDING'").Scan(&count); err != nil || count != 1000 {
		t.Fatalf("pending=%d err=%v", count, err)
	}
	// Binding and same-key retries work at capacity without extending creation.
	id := fmt.Sprintf("node_%064x", 1)
	key := bytes.Repeat([]byte{100}, 32)
	if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET created_at=now()-interval '6 days' WHERE public_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetByPublicID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		got, err := s.RegisterEnrollment(ctx, id, "retry", key)
		if err != nil || !reflect.DeepEqual(before, got) {
			t.Fatalf("retry mutated record: %+v %v", got, err)
		}
	}
	if _, err := s.RegisterEnrollment(ctx, id, "takeover", bytes.Repeat([]byte{101}, 32)); !errors.Is(err, nodes.ErrEnrollmentConflict) {
		t.Fatalf("pinned key takeover: %v", err)
	}
}

func TestHandshakeExpiredPendingRecreatedWithFreshIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := nodes.NewPublicID()
	old, err := s.RegisterEnrollment(ctx, id, "old", bytes.Repeat([]byte{10}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET created_at=now()-interval '7 days' WHERE public_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	// Real FK dependants must be removed by cleanup, not block admission or
	// become attached to the new row that reuses the public ID.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO relay_revocation_events(node_id) VALUES($1)`, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO relay_revocation_deliveries(node_id,relay_id) VALUES($1,'test')`, old.ID); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{11}, 32)
	fresh, err := s.RegisterEnrollment(ctx, id, "fresh", key)
	if err != nil {
		t.Fatalf("expired ID could not be reused: %v", err)
	}
	if fresh.ID == old.ID || fresh.SoftwareVersion != "fresh" || !fresh.CreatedAt.After(old.CreatedAt) {
		t.Fatalf("not a fresh record: %+v", fresh)
	}
	for _, table := range []string{"relay_revocation_events", "relay_revocation_deliveries"} {
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE node_id=$1", old.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("stale %s count=%d err=%v", table, count, err)
		}
	}
	_, got, err := s.EnrollmentIdentity(ctx, id)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("new key not pinned: %v", err)
	}
}

func TestHandshakeExpiryCleanupDurableOnConflict(t *testing.T) {
	for _, reason := range []string{"quota", "approved", "rejected", "revoked", "pinned", "duplicate-key"} {
		t.Run(reason, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			if err := s.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			id, _ := nodes.NewPublicID()
			owner, _ := nodes.NewPublicID()
			key := bytes.Repeat([]byte{20}, 32)
			if _, err := s.RegisterEnrollment(ctx, owner, "owner", key); err != nil {
				t.Fatal(err)
			}
			decided := reason == "approved" || reason == "rejected" || reason == "revoked"
			if reason == "pinned" || decided {
				id = owner
				key = bytes.Repeat([]byte{21}, 32)
			}
			if decided {
				decide := s.Approve
				if reason == "rejected" {
					decide = s.Reject
				}
				if _, err := decide(ctx, owner); err != nil {
					t.Fatal(err)
				}
				if reason == "revoked" {
					if _, err := s.Revoke(ctx, owner); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET created_at=now()-interval '8 days' WHERE public_id=$1`, owner); err != nil {
					t.Fatal(err)
				}
			}
			if reason == "quota" {
				if _, err := s.db.ExecContext(ctx, `INSERT INTO nodes(public_id,software_version) SELECT 'node_'||lpad(to_hex(i),64,'0'),'seed' FROM generate_series(1,999) i`); err != nil {
					t.Fatal(err)
				}
			}
			expired, _ := nodes.NewPublicID()
			if _, err := s.db.ExecContext(ctx, `INSERT INTO nodes(public_id,software_version,created_at) VALUES($1,'expired',now()-interval '7 days')`, expired); err != nil {
				t.Fatal(err)
			}
			before, stored, err := s.EnrollmentIdentity(ctx, owner)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.RegisterEnrollment(ctx, id, "attempt", key); !errors.Is(err, nodes.ErrEnrollmentConflict) {
				t.Fatalf("conflict=%v", err)
			}
			if _, err := s.GetByPublicID(ctx, expired); !errors.Is(err, nodes.ErrNotFound) {
				t.Fatalf("cleanup rolled back on %s conflict: %v", reason, err)
			}
			after, got, err := s.EnrollmentIdentity(ctx, owner)
			if err != nil || !reflect.DeepEqual(before, after) || !bytes.Equal(stored, got) {
				t.Fatalf("existing identity mutated: %v", err)
			}
			if id != owner {
				if _, err := s.GetByPublicID(ctx, id); !errors.Is(err, nodes.ErrNotFound) {
					t.Fatalf("failed registration left row: %v", err)
				}
			}
		})
	}
}

func TestHandshakeExpiredPendingCannotBeApproved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := nodes.NewPublicID()
	if _, err := s.RegisterEnrollment(ctx, id, "original", bytes.Repeat([]byte{30}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET created_at=now()-interval '7 days' WHERE public_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetByPublicID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, id); !errors.Is(err, nodes.ErrEnrollmentConflict) {
		t.Fatalf("expired approval error=%v", err)
	}
	after, err := s.GetByPublicID(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("expired approval mutated record: %v", err)
	}
}

func TestHandshakeListPendingExcludesExpiredBeforeCleanup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Seed directly: no RegisterEnrollment call may clean up expired rows.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO nodes(public_id,software_version,created_at)
		SELECT 'node_' || lpad(to_hex(i),64,'0'), 'seed',
		CASE i WHEN 1 THEN statement_timestamp()-interval '8 days'
		       WHEN 2 THEN statement_timestamp()-interval '7 days'
		       WHEN 3 THEN statement_timestamp()-interval '6 days'
		       ELSE statement_timestamp() END
		FROM generate_series(1,5) i`); err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct {
		after string
		limit int
		want  []string
	}{
		{"", 1, []string{fmt.Sprintf("node_%064x", 3), fmt.Sprintf("node_%064x", 4)}},
		{fmt.Sprintf("node_%064x", 3), 1, []string{fmt.Sprintf("node_%064x", 4), fmt.Sprintf("node_%064x", 5)}},
		{"", 100, []string{fmt.Sprintf("node_%064x", 3), fmt.Sprintf("node_%064x", 4), fmt.Sprintf("node_%064x", 5)}},
	} {
		got, err := s.ListPending(ctx, page.after, page.limit)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(got))
		for _, node := range got {
			ids = append(ids, node.PublicID)
		}
		if !reflect.DeepEqual(ids, page.want) {
			t.Fatalf("pending before cleanup after=%q limit=%d: got=%v want=%v", page.after, page.limit, ids, page.want)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM nodes").Scan(&count); err != nil || count != 5 {
		t.Fatalf("listing must not delete expired rows: count=%d err=%v", count, err)
	}
}

func TestHandshakeExpiredPendingIdentityHidden(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := nodes.NewPublicID()
	if _, err := s.RegisterEnrollment(ctx, id, "original", bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET created_at=now()-interval '7 days' WHERE public_id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, key, err := s.EnrollmentIdentity(ctx, id); !errors.Is(err, nodes.ErrNotFound) || key != nil {
		t.Fatalf("expired identity exposed: key length=%d error=%v", len(key), err)
	}
}
