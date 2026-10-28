package postgres

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/media"
	"arknet/core/internal/nodes"
	"arknet/core/internal/routing"
	"arknet/core/internal/store"
)

func TestHomeNodeStatusUsesCoreObservedHealthAndVerifiedReplicas(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	nodeID, err := nodes.NewPublicID()
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{61}, 32)
	if _, err := s.RegisterEnrollment(ctx, nodeID, "test", key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, nodeID); err != nil {
		t.Fatal(err)
	}
	present, err := s.CreateMediaObject(ctx, store.Object{ObjectType: media.ObjectTypeVideo, Size: 64, InfoHashV2: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	missing, err := s.CreateMediaObject(ctx, store.Object{ObjectType: media.ObjectTypeVideo, Size: 64, InfoHashV2: strings.Repeat("d", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReportMediaReplicaAvailable(ctx, nodeID, present.ID, present.Size, present.InfoHashV2); err != nil {
		t.Fatal(err)
	}
	status := func(mediaID string) routing.HomeNodeStatus {
		t.Helper()
		got, err := s.HomeNodeStatus(ctx, nodeID, mediaID, routing.MaxHeartbeatAge)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := status(present.ID); got.Healthy || !got.VerifiedAvailable {
		t.Fatalf("approved but never heartbeated: %+v", got)
	}
	proof := time.Now().Unix()
	heartbeat := func(state string, healthy, databaseHealthy bool) {
		t.Helper()
		proof++
		if err := s.AcceptHeartbeat(ctx, nodeID, "test", state, healthy, databaseHealthy, false, key, proof); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat("CONNECTED", true, true)
	if got := status(present.ID); !got.Healthy || !got.VerifiedAvailable {
		t.Fatalf("healthy with replica: %+v", got)
	}
	// Peer Serving is off: routing to one's own home Node does not depend on it.
	if got := status(missing.ID); !got.Healthy || got.VerifiedAvailable {
		t.Fatalf("missing media: %+v", got)
	}
	for _, unhealthy := range []struct {
		state       string
		healthy, db bool
	}{{"DEGRADED", true, true}, {"RECONNECTING", true, true}, {"CONNECTED", false, true}, {"CONNECTED", true, false}} {
		heartbeat(unhealthy.state, unhealthy.healthy, unhealthy.db)
		if got := status(present.ID); got.Healthy {
			t.Fatalf("%+v reported healthy", unhealthy)
		}
	}
	heartbeat("MESH", true, true)
	if got := status(present.ID); !got.Healthy {
		t.Fatalf("MESH not healthy: %+v", got)
	}
	// Offline: no accepted heartbeat within the freshness bound.
	if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET last_seen_at = clock_timestamp() - interval '5 minutes' WHERE public_id=$1", nodeID); err != nil {
		t.Fatal(err)
	}
	if got := status(present.ID); got.Healthy {
		t.Fatalf("stale heartbeat healthy: %+v", got)
	}
	heartbeat("CONNECTED", true, true)
	if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET trust_status='BLOCKED' WHERE public_id=$1", nodeID); err != nil {
		t.Fatal(err)
	}
	if got := status(present.ID); got.Healthy {
		t.Fatalf("blocked Node healthy: %+v", got)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET trust_status='TRUSTED' WHERE public_id=$1", nodeID); err != nil {
		t.Fatal(err)
	}
	// A removed object is never available through a Node, even with its
	// retained replica row.
	if _, err := s.UpdateMediaObjectState(ctx, present.ID, media.ObjectRemoved); err != nil {
		t.Fatal(err)
	}
	if got := status(present.ID); !got.Healthy || got.VerifiedAvailable {
		t.Fatalf("removed media available: %+v", got)
	}
	unknownNode := "node_" + strings.Repeat("e", 64)
	if got, err := s.HomeNodeStatus(ctx, unknownNode, missing.ID, routing.MaxHeartbeatAge); err != nil || got.Healthy || got.VerifiedAvailable {
		t.Fatalf("unknown node: %+v %v", got, err)
	}
}
