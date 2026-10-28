package postgres

import (
	"arknet/core/internal/nodes"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"reflect"
	"sync"

	"testing"
)

func TestHandshakeInvalidKeyDoesNotAccessDatabase(t *testing.T) {
	id, _ := nodes.NewPublicID()
	s := &Store{}
	for _, key := range [][]byte{nil, make([]byte, 31), make([]byte, 33)} {
		if _, err := s.RegisterEnrollment(context.Background(), id, "v", key); !errors.Is(err, nodes.ErrEnrollmentConflict) {
			t.Fatalf("invalid key: %v", err)
		}
	}
}
func TestHandshakePinsKeyAndPreservesMetadata(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := nodes.NewPublicID()
	key := bytes.Repeat([]byte{1}, 32)
	original, err := s.Enroll(ctx, id, "original")
	if err != nil {
		t.Fatal(err)
	}
	registered, err := s.RegisterEnrollment(ctx, id, "replacement", key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, registered) {
		t.Fatal("binding changed metadata")
	}
	retry, err := s.RegisterEnrollment(ctx, id, "retry", key)
	if err != nil || !reflect.DeepEqual(original, retry) {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	if _, err := s.RegisterEnrollment(ctx, id, "v", bytes.Repeat([]byte{2}, 32)); !errors.Is(err, nodes.ErrEnrollmentConflict) {
		t.Fatalf("replacement: %v", err)
	}
	other, _ := nodes.NewPublicID()
	if _, err := s.RegisterEnrollment(ctx, other, "v", key); !errors.Is(err, nodes.ErrEnrollmentConflict) {
		t.Fatalf("key reused: %v", err)
	}
	node, stored, err := s.EnrollmentIdentity(ctx, id)
	if err != nil || !reflect.DeepEqual(node, original) || !bytes.Equal(stored, key) {
		t.Fatalf("identity: %+v %x %v", node, stored, err)
	}
	if _, err := s.GetByPublicID(ctx, other); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatal("conflict left partial row")
	}
}

func TestHandshakeDecidedNodesNeverBindOrReset(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, decide := range []func(context.Context, string) (nodes.Node, error){s.Approve, s.Reject} {
		for _, bound := range []bool{false, true} {
			id, _ := nodes.NewPublicID()
			key := bytes.Repeat([]byte{3}, 32)
			if bound {
				if _, err := rand.Read(key); err != nil {
					t.Fatal(err)
				}
				if _, err := s.RegisterEnrollment(ctx, id, "v", key); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.Enroll(ctx, id, "v"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := decide(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			_, originalKey, err := s.EnrollmentIdentity(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, attempt := range [][]byte{key, bytes.Repeat([]byte{4}, 32)} {
				if _, err := s.RegisterEnrollment(ctx, id, "replacement", attempt); !errors.Is(err, nodes.ErrEnrollmentConflict) {
					t.Fatalf("decided registration: %v", err)
				}
			}
			after, stored, err := s.EnrollmentIdentity(ctx, id)
			if err != nil || !reflect.DeepEqual(before, after) || !bytes.Equal(originalKey, stored) {
				t.Fatalf("decision changed: %v", err)
			}
		}
	}
}
func TestHandshakeConcurrentFirstBindingHasOneWinner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := nodes.NewPublicID()
	if _, err := s.Enroll(ctx, id, "original"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.RegisterEnrollment(ctx, id, "v", bytes.Repeat([]byte{byte(i + 1)}, 32))
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
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	node, key, err := s.EnrollmentIdentity(ctx, id)
	if err != nil || node.SoftwareVersion != "original" || (key[0] != 1 && key[0] != 2) {
		t.Fatalf("binding: %+v %x %v", node, key, err)
	}
}
func TestHandshakeApprovalWinsBeforeFirstBinding(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := nodes.NewPublicID()
	if _, err := s.Enroll(ctx, id, "v"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET enrollment_status='APPROVED',trust_status='TRUSTED' WHERE public_id=$1", id); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, err := s.RegisterEnrollment(ctx, id, "v", bytes.Repeat([]byte{5}, 32))
		result <- err
	}()
	<-started
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, nodes.ErrEnrollmentConflict) {
		t.Fatalf("first bind after approval: %v", err)
	}
	node, key, err := s.EnrollmentIdentity(ctx, id)
	if err != nil || key != nil || node.EnrollmentStatus != nodes.EnrollmentApproved || node.TrustStatus != nodes.TrustTrusted {
		t.Fatalf("unsafe binding: %+v %x %v", node, key, err)
	}
}
func TestHandshakeCredentialsSurviveStorageReopen(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := nodes.NewPublicID()
	key := bytes.Repeat([]byte{6}, 32)
	if _, err := s.RegisterEnrollment(ctx, id, "v", key); err != nil {
		t.Fatal(err)
	}
	before, err := s.Approve(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := s.db.QueryRowContext(ctx, "SHOW search_path").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("ARKNET_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	node, stored, err := reopened.EnrollmentIdentity(ctx, id)
	if err != nil || !reflect.DeepEqual(before, node) || !bytes.Equal(key, stored) {
		t.Fatalf("reopen: %+v %x %v", node, stored, err)
	}
}
func TestHandshakeDatabaseEnforcesCredentialConstraints(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := nodes.NewPublicID()
	b, _ := nodes.NewPublicID()
	key := bytes.Repeat([]byte{7}, 32)
	if _, err := s.RegisterEnrollment(ctx, a, "v", key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, b, "v"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{key, make([]byte, 31), make([]byte, 33)} {
		if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET enrollment_public_key=$2 WHERE public_id=$1", b, invalid); err == nil {
			t.Fatal("database accepted duplicate or invalid length key")
		}
	}
}

func TestHandshakeConcurrentUniqueKeyOwnership(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := nodes.NewPublicID()
	b, _ := nodes.NewPublicID()
	key := bytes.Repeat([]byte{8}, 32)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{a, b} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			_, err := s.RegisterEnrollment(ctx, id, "v", key)
			results <- err
		}(id)
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
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM nodes WHERE public_id IN ($1,$2)", a, b).Scan(&count); err != nil || count != 1 {
		t.Fatalf("partial rows: %d %v", count, err)
	}
}
func TestHandshakeConcurrentApprovalAndFirstBinding(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		id, _ := nodes.NewPublicID()
		key := bytes.Repeat([]byte{byte(i + 10)}, 32)
		if _, err := s.Enroll(ctx, id, "v"); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		binding := make(chan error, 1)
		approval := make(chan error, 1)
		go func() { <-start; _, err := s.RegisterEnrollment(ctx, id, "v", key); binding <- err }()
		go func() { <-start; _, err := s.Approve(ctx, id); approval <- err }()
		close(start)
		bindErr := <-binding
		if err := <-approval; err != nil {
			t.Fatal(err)
		}
		node, stored, err := s.EnrollmentIdentity(ctx, id)
		if err != nil || node.EnrollmentStatus != nodes.EnrollmentApproved || node.TrustStatus != nodes.TrustTrusted {
			t.Fatalf("decision: %+v %v", node, err)
		}
		if bindErr == nil {
			if !bytes.Equal(stored, key) {
				t.Fatal("winning binding lost key")
			}
		} else if !errors.Is(bindErr, nodes.ErrEnrollmentConflict) || stored != nil {
			t.Fatalf("approval-winning key bound: %x %v", stored, bindErr)
		}
	}
}
