package postgres

import (
	"arknet/core/internal/nodes"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func heartbeatStore(t *testing.T) (*Store, nodes.HeartbeatRepository, string, []byte) {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo, ok := any(s).(nodes.HeartbeatRepository)
	if !ok {
		t.Fatal("Store does not implement HeartbeatRepository")
	}
	id, _ := nodes.NewPublicID()
	key := bytes.Repeat([]byte{42}, 32)
	if _, err := s.RegisterEnrollment(ctx, id, "old", key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, id); err != nil {
		t.Fatal(err)
	}
	return s, repo, id, key
}
func TestHeartbeatStoreBoundary(t *testing.T) {
	if _, ok := any(&Store{}).(nodes.HeartbeatRepository); !ok {
		t.Fatal("Store does not implement HeartbeatRepository")
	}
}
func TestHeartbeatAtomicUpdateAndReplay(t *testing.T) {
	s, repo, id, key := heartbeatStore(t)
	ctx := context.Background()
	proof := time.Now().Unix()
	before, _ := s.GetByPublicID(ctx, id)
	if err := repo.AcceptHeartbeat(ctx, id, "v2", "CONNECTED", true, false, true, key, proof); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetByPublicID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastSeenAt == nil || after.SoftwareVersion != "v2" || !after.CreatedAt.Equal(before.CreatedAt) || after.ID != before.ID || after.EnrollmentStatus != before.EnrollmentStatus || after.TrustStatus != before.TrustStatus {
		t.Fatalf("bad update %+v", after)
	}
	var state string
	var health []byte
	var stored int64
	var peerServing bool
	if err := s.db.QueryRowContext(ctx, "SELECT current_state,reported_health,heartbeat_proof_at,peer_serving FROM nodes WHERE public_id=$1", id).Scan(&state, &health, &stored, &peerServing); err != nil {
		t.Fatal(err)
	}
	var h map[string]bool
	if json.Unmarshal(health, &h) != nil || len(h) != 2 || !h["healthy"] || h["database_healthy"] || state != "CONNECTED" || stored != proof || !peerServing {
		t.Fatalf("metadata: %s %s %d", state, health, stored)
	}
	for _, ts := range []int64{proof, proof - 1} {
		if err := repo.AcceptHeartbeat(ctx, id, "evil", "OFFLINE", false, true, false, key, ts); err == nil {
			t.Fatal("replay accepted")
		}
		current, _ := s.GetByPublicID(ctx, id)
		if !reflect.DeepEqual(after, current) {
			t.Fatal("replay changed metadata")
		}
	}
	if err := repo.AcceptHeartbeat(ctx, id, "v3", "MESH", false, true, false, key, proof+1); err != nil {
		t.Fatal(err)
	}
}
func TestHeartbeatConcurrentReplayHasOneWinner(t *testing.T) {
	s, repo, id, key := heartbeatStore(t)
	ctx := context.Background()
	proof := time.Now().Unix()
	start := make(chan struct{})
	results := make(chan error, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- repo.AcceptHeartbeat(ctx, id, "race", "MESH", true, true, false, key, proof)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("winners=%d", wins)
	}
	node, err := s.GetByPublicID(ctx, id)
	if err != nil || node.SoftwareVersion != "race" || node.LastSeenAt == nil {
		t.Fatalf("node %+v %v", node, err)
	}
}
func TestHeartbeatConcurrentGuardChangesCannotWrite(t *testing.T) {
	for _, tt := range []struct {
		name, sql string
		want      error
	}{
		{"key", "UPDATE nodes SET enrollment_public_key=decode(repeat('2b',32),'hex') WHERE public_id=$1", nodes.ErrNodeAuthentication},
		{"trust", "UPDATE nodes SET trust_status='UNTRUSTED' WHERE public_id=$1", nodes.ErrNodeUnapproved},
		{"proof", "UPDATE nodes SET heartbeat_proof_at=100 WHERE public_id=$1", nodes.ErrHeartbeatReplay},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, repo, id, key := heartbeatStore(t)
			ctx := context.Background()
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, tt.sql, id); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- repo.AcceptHeartbeat(ctx, id, "attacker", "OFFLINE", false, false, false, key, 100) }()
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, tt.want) {
				t.Fatalf("guard race: %v want %v", err, tt.want)
			}
			node, err := s.GetByPublicID(ctx, id)
			if err != nil || node.LastSeenAt != nil || node.SoftwareVersion != "old" || node.CurrentState != nil || node.ReportedHealth != nil {
				t.Fatalf("guard metadata %+v %v", node, err)
			}
		})
	}
}
func TestHeartbeatConcurrentIncreasingProofKeepsNewestMetadata(t *testing.T) {
	s, repo, id, key := heartbeatStore(t)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 20)
	for i := range 20 {
		go func(i int) {
			<-start
			results <- repo.AcceptHeartbeat(ctx, id, fmt.Sprint(i), "MESH", true, true, false, key, int64(100+i))
		}(i)
	}
	close(start)
	for range 20 {
		err := <-results
		if err != nil && !errors.Is(err, nodes.ErrHeartbeatReplay) {
			t.Fatal(err)
		}
	}
	node, err := s.GetByPublicID(ctx, id)
	if err != nil || node.SoftwareVersion != "19" {
		t.Fatalf("newest metadata %+v %v", node, err)
	}
	var proof int64
	if err := s.db.QueryRowContext(ctx, "SELECT heartbeat_proof_at FROM nodes WHERE public_id=$1", id).Scan(&proof); err != nil || proof != 119 {
		t.Fatalf("proof=%d %v", proof, err)
	}
}

func TestHeartbeatAuthorizationGuardsPreserveMetadata(t *testing.T) {
	s, repo, id, key := heartbeatStore(t)
	ctx := context.Background()
	before, _ := s.GetByPublicID(ctx, id)
	for _, bad := range [][]byte{nil, bytes.Repeat([]byte{43}, 32)} {
		if err := repo.AcceptHeartbeat(ctx, id, "wrong", "OFFLINE", false, false, false, bad, 1); err == nil {
			t.Fatal("wrong key accepted")
		}
	}
	missing, _ := nodes.NewPublicID()
	if err := repo.AcceptHeartbeat(ctx, missing, "v", "MESH", true, true, false, key, 1); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	for _, pair := range [][2]string{{"PENDING", "UNTRUSTED"}, {"REJECTED", "BLOCKED"}, {"APPROVED", "UNTRUSTED"}} {
		if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET enrollment_status=$2,trust_status=$3 WHERE public_id=$1", id, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
		if err := repo.AcceptHeartbeat(ctx, id, "wrong", "OFFLINE", true, true, false, key, 1); err == nil {
			t.Fatal("unapproved accepted")
		}
	}
	after, _ := s.GetByPublicID(ctx, id)
	if after.LastSeenAt != nil || after.SoftwareVersion != before.SoftwareVersion {
		t.Fatal("unauthorized update")
	}
}
