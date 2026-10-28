package postgres

import (
	"context"
	"strings"
	"testing"

	"arknet/core/internal/media"
	"arknet/core/internal/routing"
	"arknet/core/internal/store"
)

func TestMediaMissesAggregatePerNodeWithoutUserData(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	home, other := approvedNode(t, s, 61), approvedNode(t, s, 62)
	newObject := func(hash string) store.Object {
		t.Helper()
		object, err := s.CreateMediaObject(ctx, store.Object{ObjectType: media.ObjectTypeVideo, Size: 32, InfoHashV2: strings.Repeat(hash, 64)})
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	missed, popular, held, removed := newObject("1"), newObject("2"), newObject("3"), newObject("4")
	if _, err := s.ReportMediaReplicaAvailable(ctx, home, held.ID, held.Size, held.InfoHashV2); err != nil {
		t.Fatal(err)
	}
	record := func(node, id string, times int) {
		t.Helper()
		for i := 0; i < times; i++ {
			if err := s.RecordMediaMiss(ctx, node, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	record(home, missed.ID, 1)
	record(home, popular.ID, 3)
	record(home, held.ID, 2)    // home already has it: never recorded
	record(home, removed.ID, 1) // removed below: never listed
	record(other, missed.ID, 5) // another Node's misses stay separate
	record(home, "media_"+strings.Repeat("f", 64), 1)
	if _, err := s.UpdateMediaObjectState(ctx, removed.ID, media.ObjectRemoved); err != nil {
		t.Fatal(err)
	}
	misses, err := s.ListMediaMisses(ctx, home, routing.MaxListedMisses)
	if err != nil {
		t.Fatal(err)
	}
	if len(misses) != 2 || misses[0] != (routing.MediaMiss{MediaID: popular.ID, MissCount: 3}) || misses[1] != (routing.MediaMiss{MediaID: missed.ID, MissCount: 1}) {
		t.Fatalf("home misses = %+v", misses)
	}
	var heldRows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM media_miss_events e JOIN media_objects m ON m.id=e.media_object_id WHERE m.public_id=$1`, held.ID).Scan(&heldRows); err != nil || heldRows != 0 {
		t.Fatalf("held media recorded: %d %v", heldRows, err)
	}
	// Once the Node verifies the object, it disappears from its misses.
	if _, err := s.ReportMediaReplicaAvailable(ctx, home, popular.ID, popular.Size, popular.InfoHashV2); err != nil {
		t.Fatal(err)
	}
	misses, _ = s.ListMediaMisses(ctx, home, routing.MaxListedMisses)
	if len(misses) != 1 || misses[0].MediaID != missed.ID {
		t.Fatalf("resolved miss still listed: %+v", misses)
	}
	if others, _ := s.ListMediaMisses(ctx, other, routing.MaxListedMisses); len(others) != 1 || others[0].MissCount != 5 {
		t.Fatalf("other node misses = %+v", others)
	}
	// An untrusted Node neither records nor reads misses.
	if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET trust_status='BLOCKED' WHERE public_id=$1", home); err != nil {
		t.Fatal(err)
	}
	if misses, _ := s.ListMediaMisses(ctx, home, routing.MaxListedMisses); len(misses) != 0 {
		t.Fatalf("blocked node reads misses: %+v", misses)
	}
	var columns string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(column_name, ',' ORDER BY column_name) FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'media_miss_events'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != "first_missed_at,last_missed_at,media_object_id,miss_count,node_id" {
		t.Fatalf("miss columns = %s", columns)
	}
}
