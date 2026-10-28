package postgres

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/mediaselection"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func mediaObject(character string, size int64) catalog.Object {
	now := time.Now().UTC()
	return catalog.Object{ID: "media_" + strings.Repeat(character, 64), Title: "Selected media", Size: size, State: catalog.Active, ContentType: catalog.Video, CreatedAt: now, UpdatedAt: now}
}

func TestMediaQueueBudgetAndIdempotency(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	first := mediaObject("a", 10)
	for _, budget := range []int64{-2, 0, 9} {
		if _, err := s.QueueMediaSelection(ctx, first, budget); !errors.Is(err, mediaselection.ErrAllocationExceeded) {
			t.Fatalf("budget %d = %v", budget, err)
		}
	}
	got, err := s.QueueMediaSelection(ctx, first, 10)
	if err != nil {
		t.Fatal(err)
	}
	first.Size = 100
	first.Title = "New remote metadata"
	for _, budget := range []int64{0, 1} {
		duplicate, err := s.QueueMediaSelection(ctx, first, budget)
		if err != nil || duplicate != got {
			t.Fatalf("duplicate changed reservation: %+v %v", duplicate, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE media_selections SET local_state='ERROR' WHERE id=$1", first.ID); err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.QueueMediaSelection(ctx, first, 10)
	if err != nil || duplicate.LocalState != mediaselection.Error || duplicate.Size != 10 {
		t.Fatalf("reset existing status: %+v %v", duplicate, err)
	}
	if _, err := s.QueueMediaSelection(ctx, mediaObject("b", 1), 10); !errors.Is(err, mediaselection.ErrAllocationExceeded) {
		t.Fatalf("full allocation = %v", err)
	}
	if _, err := s.QueueMediaSelection(ctx, mediaObject("b", math.MaxInt64), math.MaxInt64); !errors.Is(err, mediaselection.ErrAllocationExceeded) {
		t.Fatalf("overflow allocation = %v", err)
	}
	if _, err := s.QueueMediaSelection(ctx, mediaObject("c", 0), 0); !errors.Is(err, mediaselection.ErrAllocationExceeded) {
		t.Fatalf("zero disables zero-size new selection: %v", err)
	}
	objects, err := s.ListMediaSelections(ctx)
	if err != nil || len(objects) != 1 {
		t.Fatalf("rejected rows inserted: %+v %v", objects, err)
	}
}

func TestMediaQueueUnlimitedAllocation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	first := mediaObject("a", 10)
	if _, err := s.QueueMediaSelection(ctx, first, 10); err != nil {
		t.Fatal(err)
	}
	second := mediaObject("b", 20)
	queued, err := s.QueueMediaSelection(ctx, second, -1)
	if err != nil || queued.LocalState != mediaselection.Queued {
		t.Fatalf("unlimited queue: %+v, %v", queued, err)
	}
	duplicate, err := s.QueueMediaSelection(ctx, second, -1)
	if err != nil || duplicate != queued {
		t.Fatalf("unlimited duplicate: %+v, %v", duplicate, err)
	}
	if _, err := s.QueueMediaSelection(ctx, mediaObject("c", 0), -1); err != nil {
		t.Fatalf("unlimited zero-size: %v", err)
	}
	removed := mediaObject("d", 1)
	removed.State = catalog.Removed
	if _, err := s.QueueMediaSelection(ctx, removed, -1); !errors.Is(err, mediaselection.ErrRemoved) {
		t.Fatalf("unlimited bypassed removal: %v", err)
	}
	invalid := mediaObject("d", -1)
	if _, err := s.QueueMediaSelection(ctx, invalid, -1); !errors.Is(err, mediaselection.ErrInvalidObject) {
		t.Fatalf("unlimited bypassed validation: %v", err)
	}
	objects, err := s.ListMediaSelections(ctx)
	if err != nil || len(objects) != 3 {
		t.Fatalf("unlimited selections: %+v, %v", objects, err)
	}
	// Going back to a finite allocation does not erase existing reservations.
	if _, err := s.QueueMediaSelection(ctx, mediaObject("e", 1), 10); !errors.Is(err, mediaselection.ErrAllocationExceeded) {
		t.Fatalf("finite allocation after unlimited: %v", err)
	}
}

func TestMediaQueueConcurrentBudget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			_, err := s.QueueMediaSelection(ctx, mediaObject(id, 6), 10)
			results <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, rejected := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, mediaselection.ErrAllocationExceeded) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("competing queues: %d successes, %d rejected", successes, rejected)
	}
}

func TestMediaQueueRejectsRemovedAndInvalid(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	removed := mediaObject("a", 1)
	removed.State = catalog.Removed
	if _, err := s.QueueMediaSelection(ctx, removed, 100); !errors.Is(err, mediaselection.ErrRemoved) {
		t.Fatalf("removed = %v", err)
	}
	mutations := []func(*catalog.Object){
		func(o *catalog.Object) { o.ID = "../../file" },
		func(o *catalog.Object) { o.ID = "media_" + strings.Repeat("A", 64) },
		func(o *catalog.Object) { o.Size = -1 },
		func(o *catalog.Object) { o.State = "INVALID" },
		func(o *catalog.Object) { o.ContentType = "INVALID" },
		func(o *catalog.Object) { o.InfoHashV2 = "not-a-hash" },
		func(o *catalog.Object) { o.CreatedAt = time.Time{} },
		func(o *catalog.Object) { o.UpdatedAt = o.CreatedAt.Add(-time.Second) },
	}
	for i, mutate := range mutations {
		object := mediaObject("b", 1)
		mutate(&object)
		if _, err := s.QueueMediaSelection(ctx, object, 100); !errors.Is(err, mediaselection.ErrInvalidObject) {
			t.Fatalf("invalid case %d = %v", i, err)
		}
	}
	objects, err := s.ListMediaSelections(ctx)
	if err != nil || len(objects) != 0 {
		t.Fatalf("invalid/removed persisted: %+v %v", objects, err)
	}
}

var _ mediaselection.Repository = (*Store)(nil)

func TestMediaExplicitTombstonesRetained(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	first, missing := mediaObject("a", 4), mediaObject("b", 6)
	for _, object := range []catalog.Object{first, missing} {
		if _, err := s.QueueMediaSelection(ctx, object, 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TransitionMediaSelection(ctx, first.ID, mediaselection.Queued, mediaselection.Available); err != nil {
		t.Fatal(err)
	}
	// An ACTIVE entry and a missing entry cannot imply removal.
	if err := s.RetainRemovedMediaSelections(ctx, []catalog.Object{first}); err != nil {
		t.Fatal(err)
	}
	objects, err := s.ListMediaSelections(ctx)
	if err != nil || len(objects) != 2 || objects[0].LocalState != mediaselection.Available || objects[1].LocalState != mediaselection.Queued {
		t.Fatalf("missing/active changed: %+v %v", objects, err)
	}
	first.State = catalog.Removed
	first.Size = 99
	first.Title = "Remote tombstone"
	unselected := mediaObject("c", 100)
	unselected.State = catalog.Removed
	for i := 0; i < 2; i++ {
		if err := s.RetainRemovedMediaSelections(ctx, []catalog.Object{first, unselected}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err = s.ListMediaSelections(ctx)
	if err != nil || len(objects) != 2 {
		t.Fatalf("tombstone list: %+v %v", objects, err)
	}
	if objects[0].State != catalog.Removed || objects[0].LocalState != mediaselection.RemovedRetained || objects[0].Size != 4 || objects[0].Title == first.Title {
		t.Fatalf("retained selection corrupted: %+v", objects[0])
	}
	if objects[1].State != catalog.Active || objects[1].LocalState != mediaselection.Queued {
		t.Fatalf("missing entry changed: %+v", objects[1])
	}
	if _, err := s.QueueMediaSelection(ctx, first, 100); !errors.Is(err, mediaselection.ErrRemoved) {
		t.Fatalf("removed retry = %v", err)
	}
	first.State = catalog.Active
	if _, err := s.QueueMediaSelection(ctx, first, 100); !errors.Is(err, mediaselection.ErrRemoved) {
		t.Fatalf("retained resurrected = %v", err)
	}
	if _, err := s.QueueMediaSelection(ctx, mediaObject("d", 1), 10); !errors.Is(err, mediaselection.ErrAllocationExceeded) {
		t.Fatalf("removed reservation released: %v", err)
	}
	if err := s.RetainRemovedMediaSelections(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMediaReservationSumBeyondInt64(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate retained selections after the operator lowers the allocation:
	// individually valid bigint sizes can have a sum larger than bigint.
	for _, id := range []string{"a", "b"} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO media_selections (id,title,size,state,local_state) VALUES ($1,'retained',$2,'REMOVED','REMOVED_RETAINED')`, mediaObject(id, 0).ID, int64(math.MaxInt64)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.QueueMediaSelection(ctx, mediaObject("c", 0), math.MaxInt64); !errors.Is(err, mediaselection.ErrAllocationExceeded) {
		t.Fatalf("oversized numeric reservation sum = %v", err)
	}
}

func TestMediaDatabaseConstraints(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id           string
		size         int64
		state, local string
	}{
		{"invalid", 0, "ACTIVE", "QUEUED"},
		{mediaObject("a", 0).ID, -1, "ACTIVE", "QUEUED"},
		{mediaObject("a", 0).ID, 0, "INVALID", "QUEUED"},
		{mediaObject("a", 0).ID, 0, "ACTIVE", "INVALID"},
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO media_selections (id,title,size,state,local_state) VALUES ($1,'selection',$2,$3,$4)`, test.id, test.size, test.state, test.local); err == nil {
			t.Fatalf("database accepted invalid selection: %+v", test)
		}
	}
	for _, state := range []mediaselection.State{mediaselection.NotPresent, mediaselection.Queued, mediaselection.Downloading, mediaselection.Available, mediaselection.Error} {
		object := mediaObject("a", 1)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO media_selections (id,title,size,state,local_state) VALUES ($1,'selection',1,'ACTIVE',$2) ON CONFLICT (id) DO UPDATE SET local_state=$2`, object.ID, state); err != nil {
			t.Fatal(err)
		}
		got, err := s.QueueMediaSelection(ctx, object, 0)
		if err != nil || got.LocalState != state {
			t.Fatalf("existing %s changed: %+v %v", state, got, err)
		}
	}
}

func TestMediaQueuePersistsAcrossReopen(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := s.ListMediaSelections(ctx)
	if err != nil || objects == nil || len(objects) != 0 {
		t.Fatalf("empty selections = %#v, %v", objects, err)
	}
	object := mediaObject("a", 12)
	got, err := s.QueueMediaSelection(ctx, object, 12)
	want := mediaselection.Object{ID: object.ID, Title: object.Title, Size: 12, State: catalog.Active, LocalState: mediaselection.Queued}
	if err != nil || got != want {
		t.Fatalf("queue = %+v, %v", got, err)
	}
	var schema string
	if err := s.db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	cfg.RuntimeParams["search_path"] = schema
	s.db = stdlib.OpenDB(*cfg)
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err = s.ListMediaSelections(ctx)
	if err != nil || len(objects) != 1 || objects[0] != want {
		t.Fatalf("reopened selections = %+v, %v", objects, err)
	}
}
