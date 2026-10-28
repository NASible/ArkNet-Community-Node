package enrollment

import (
	"arknet/node-agent/internal/localstate"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubmitAcceptsExactly4KBResponse(t *testing.T) {
	store := &memoryStore{snapshot: localstate.Snapshot{NodeID: "node_" + strings.Repeat("a", 64), State: localstate.Provisioning}}
	ack, _ := json.Marshal(map[string]string{"node_id": store.snapshot.NodeID, "enrollment_status": "PENDING"})
	response := string(ack) + strings.Repeat(" ", 4096-len(ack))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(response)) }))
	defer server.Close()
	if err := Submit(context.Background(), server.URL, "test", store); err != nil {
		t.Fatal(err)
	}
	if store.records != 1 {
		t.Fatal("4KB ack not recorded")
	}
}
func TestSubmitBoundsWholeOperationTo10Seconds(t *testing.T) {
	started := time.Now()
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	defer server.Close()
	defer close(release)
	store := &memoryStore{snapshot: localstate.Snapshot{State: localstate.Provisioning}}
	done := make(chan error, 1)
	go func() { done <- Submit(context.Background(), server.URL, "test", store) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	select {
	case err := <-done:
		if err != ErrRequestFailed {
			t.Fatal(err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("10s bound ignored")
	}
	elapsed := time.Since(started)
	if elapsed < 9*time.Second || elapsed > 12*time.Second {
		t.Fatalf("unexpected deadline %s", elapsed)
	}
	if store.records != 0 {
		t.Fatal("timed-out request mutated state")
	}
}
