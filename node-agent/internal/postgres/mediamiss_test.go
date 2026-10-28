package postgres

import (
	"context"
	"errors"
	"testing"

	"arknet/node-agent/internal/mediamiss"
)

func TestMediaMissPolicyAndNotificationsPersistLocally(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if policy, err := s.MediaMissPolicy(ctx); err != nil || policy != mediamiss.Ask {
		t.Fatalf("default policy = %s %v", policy, err)
	}
	if err := s.SetMediaMissPolicy(ctx, mediamiss.Automatic); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if policy, _ := s.MediaMissPolicy(ctx); policy != mediamiss.Automatic {
		t.Fatalf("persisted policy = %s", policy)
	}
	if err := s.SetMediaMissPolicy(ctx, "NEVER"); err == nil {
		t.Fatal("invalid policy saved")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE node_configuration SET settings = jsonb_set(settings, '{media_miss_policy}', '"NEVER"')`); err == nil {
		t.Fatal("schema accepted invalid policy")
	}

	queued, ignored, fresh := mediaObject("a", 10), mediaObject("b", 20), mediaObject("c", 30)
	store := func(counts map[string]int64) {
		t.Helper()
		var ns []mediamiss.Notification
		for _, o := range []struct {
			id    string
			title string
			size  int64
		}{{queued.ID, queued.Title, queued.Size}, {ignored.ID, ignored.Title, ignored.Size}, {fresh.ID, fresh.Title, fresh.Size}} {
			ns = append(ns, mediamiss.Notification{MediaID: o.id, Title: o.title, Size: o.size, MissCount: counts[o.id]})
		}
		if err := s.StoreMediaMisses(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	store(map[string]int64{queued.ID: 1, ignored.ID: 2, fresh.ID: 3})
	pending, err := s.PendingMediaMisses(ctx)
	if err != nil || len(pending) != 3 || pending[0].MediaID != fresh.ID || pending[0].MissCount != 3 || pending[0].Size != 30 {
		t.Fatalf("pending = %+v %v", pending, err)
	}
	if err := s.IgnoreMediaMiss(ctx, ignored.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.IgnoreMediaMiss(ctx, ignored.ID); !errors.Is(err, mediamiss.ErrNotFound) {
		t.Fatalf("repeat ignore: %v", err)
	}
	if _, err := s.QueueMediaSelection(ctx, queued, -1); err != nil {
		t.Fatal(err)
	}
	// New misses refresh counts but do not resurrect ignored notifications.
	store(map[string]int64{queued.ID: 5, ignored.ID: 9, fresh.ID: 4})
	pending, _ = s.PendingMediaMisses(ctx)
	if len(pending) != 1 || pending[0].MediaID != fresh.ID || pending[0].MissCount != 4 {
		t.Fatalf("pending after queue/ignore = %+v", pending)
	}
	if err := s.StoreMediaMisses(ctx, []mediamiss.Notification{{MediaID: "../x", Title: "x", Size: 1, MissCount: 1}}); err == nil {
		t.Fatal("invalid notification stored")
	}
}
