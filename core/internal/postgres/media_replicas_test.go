package postgres

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/media"
	"arknet/core/internal/nodes"
	"arknet/core/internal/store"
)

func TestMediaReplicaReportIsDurableIdempotentAndReadable(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	node, err := db.Create(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Approve(ctx, node.PublicID); err != nil {
		t.Fatal(err)
	}
	object, err := db.CreateMediaObject(ctx, store.Object{
		ObjectType: media.ObjectTypeVideo,
		Size:       4096,
		InfoHashV2: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := db.ReportMediaReplicaAvailable(ctx, node.PublicID, object.ID, object.Size, object.InfoHashV2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.ReportMediaReplicaAvailable(ctx, node.PublicID, object.ID, object.Size, object.InfoHashV2)
	if err != nil {
		t.Fatal(err)
	}
	if first.NodeID != node.PublicID || first.MediaID != object.ID || first.AvailableAt.IsZero() {
		t.Fatalf("invalid replica: %+v", first)
	}
	if second != first {
		t.Fatalf("idempotent report changed row: first=%+v second=%+v", first, second)
	}

	got, err := db.GetMediaReplica(ctx, node.PublicID, object.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatalf("readback = %+v, want %+v", got, first)
	}
	var count int
	if err := db.db.QueryRowContext(ctx, "SELECT count(*) FROM media_replicas").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("replica rows = %d, want 1", count)
	}
}

func TestPeerServingControlsFutureSourceEligibility(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	nodeID, err := nodes.NewPublicID()
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{42}, 32)
	if _, err := db.RegisterEnrollment(ctx, nodeID, "test", key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Approve(ctx, nodeID); err != nil {
		t.Fatal(err)
	}
	object, err := db.CreateMediaObject(ctx, store.Object{ObjectType: media.ObjectTypeVideo, Size: 4096, InfoHashV2: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReportMediaReplicaAvailable(ctx, nodeID, object.ID, object.Size, object.InfoHashV2); err != nil {
		t.Fatal(err)
	}

	assertCount := func(want int) {
		t.Helper()
		sources, err := db.ListEligibleMediaSources(ctx, object.ID, 10)
		if err != nil || len(sources) != want {
			t.Fatalf("eligible sources = %+v, %v; want %d", sources, err, want)
		}
	}
	assertCount(0)
	proof := time.Now().Unix()
	if err := db.AcceptHeartbeat(ctx, nodeID, "test", "CONNECTED", true, true, true, key, proof); err != nil {
		t.Fatal(err)
	}
	assertCount(1)
	if err := db.AcceptHeartbeat(ctx, nodeID, "test", "CONNECTED", true, true, false, key, proof+1); err != nil {
		t.Fatal(err)
	}
	assertCount(0)
	if err := db.AcceptHeartbeat(ctx, nodeID, "test", "CONNECTED", true, true, true, key, proof+2); err != nil {
		t.Fatal(err)
	}
	assertCount(1)
	if _, err := db.UpdateMediaObjectState(ctx, object.ID, media.ObjectRemoved); err != nil {
		t.Fatal(err)
	}
	// The replica row remains an ArkNet fact, but a removed object is no longer
	// advertised as a source even while the Node still opts into Peer Serving.
	assertCount(0)
	if _, err := db.GetMediaReplica(ctx, nodeID, object.ID); err != nil {
		t.Fatalf("removal deleted replica record: %v", err)
	}
}
