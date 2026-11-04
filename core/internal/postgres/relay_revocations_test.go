package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"arknet/core/internal/media"
	"arknet/core/internal/relayrevocations"
	"arknet/core/internal/store"
)

var _ relayrevocations.Repository = (*Store)(nil)

func pendingFor(t *testing.T, s *Store, relays ...string) []relayrevocations.Task {
	t.Helper()
	tasks, err := s.PendingRelayRevocations(context.Background(), relays, 100)
	if err != nil {
		t.Fatal(err)
	}
	return tasks
}

func TestBlockingANodeQueuesItsRelayRevocation(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	revoked, kept := approvedNode(t, s, 1), approvedNode(t, s, 2)
	rejected := fmt.Sprintf("node_%064x", 3)
	if _, err := s.Enroll(ctx, rejected, "v"); err != nil {
		t.Fatal(err)
	}
	if got := pendingFor(t, s, "relay-a"); len(got) != 0 {
		t.Fatalf("approval queued a revocation: %v", got)
	}
	if _, err := s.Revoke(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reject(ctx, rejected); err != nil {
		t.Fatal(err)
	}
	got := pendingFor(t, s, "relay-a", "relay-b")
	var pairs []string
	for _, task := range got {
		pairs = append(pairs, task.NodeID+"@"+task.RelayID)
		if task.Attempts != 0 || task.LastError != "" || !task.LastAttemptAt.IsZero() || task.DecidedAt.IsZero() {
			t.Fatalf("fresh task: %+v", task)
		}
	}
	sort.Strings(pairs)
	want := []string{revoked + "@relay-a", revoked + "@relay-b", rejected + "@relay-a", rejected + "@relay-b"}
	sort.Strings(want)
	if strings.Join(pairs, ",") != strings.Join(want, ",") || strings.Contains(strings.Join(pairs, ","), kept) {
		t.Fatalf("pending %v", pairs)
	}
}

func TestFailedConditionalTransitionQueuesNothing(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node := approvedNode(t, s, 1)
	if _, err := s.Reject(ctx, node); err == nil {
		t.Fatal("rejected an approved Node")
	}
	if got := pendingFor(t, s, "relay-a"); len(got) != 0 {
		t.Fatalf("a failed transition queued a revocation: %v", got)
	}
}

func TestDeliveryIsRecordedPerRelayUntilAcknowledged(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node := approvedNode(t, s, 1)
	if _, err := s.Revoke(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRelayRevocationAttempt(ctx, node, "relay-a", "status_503"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRelayRevocationAttempt(ctx, node, "relay-a", "unreachable"); err != nil {
		t.Fatal(err)
	}
	got := pendingFor(t, s, "relay-a")
	if len(got) != 1 || got[0].Attempts != 2 || got[0].LastError != "unreachable" || got[0].LastAttemptAt.IsZero() {
		t.Fatalf("%+v", got)
	}
	if err := s.RecordRelayRevocationAttempt(ctx, node, "relay-a", ""); err != nil {
		t.Fatal(err)
	}
	if got := pendingFor(t, s, "relay-a"); len(got) != 0 {
		t.Fatalf("acknowledged delivery still pending: %v", got)
	}
	if got := pendingFor(t, s, "relay-b"); len(got) != 1 {
		t.Fatalf("another relay's delivery affected: %v", got)
	}
	// A relay added later still receives revocations it has not acknowledged.
	if got := pendingFor(t, s, "relay-new"); len(got) != 1 {
		t.Fatalf("new relay: %v", got)
	}
	unknown := fmt.Sprintf("node_%064x", 77)
	if err := s.RecordRelayRevocationAttempt(ctx, unknown, "relay-a", ""); err == nil {
		t.Fatal("recorded a delivery for a Node without a revocation")
	}
	for _, bad := range []string{"Relay A", strings.Repeat("a", 65), ""} {
		if err := s.RecordRelayRevocationAttempt(ctx, node, bad, ""); err == nil {
			t.Fatalf("relay ID %q accepted", bad)
		}
	}
	if err := s.RecordRelayRevocationAttempt(ctx, node, "relay-b", strings.Repeat("x", 65)); err == nil {
		t.Fatal("unbounded failure text accepted")
	}
}

func TestMediaInfoHashesIncludesRemovedObjects(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	var created []string
	for i := 1; i <= 3; i++ {
		object, err := s.CreateMediaObject(ctx, store.Object{ObjectType: media.ObjectTypeVideo, Title: fmt.Sprint("t", i), Size: 100,
			InfoHashV2: fmt.Sprintf("%064x", i)})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, object.ID)
	}
	if _, err := s.UpdateMediaObjectState(ctx, created[2], store.ObjectRemoved); err != nil {
		t.Fatal(err)
	}
	got, err := s.MediaInfoHashes(ctx)
	sort.Strings(got)
	if err != nil || len(got) != 3 || got[0] != fmt.Sprintf("%064x", 1) || got[2] != fmt.Sprintf("%064x", 3) {
		t.Fatalf("%v %v", got, err)
	}
}
