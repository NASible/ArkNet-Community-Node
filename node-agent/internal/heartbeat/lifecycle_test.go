package heartbeat

import (
	"arknet/node-agent/internal/localstate"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestRunIntervalStrictTimestampAndRecoveryMonitor(t *testing.T) {
	requests := make(chan *http.Request, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		fmt.Fprintf(w, `{"node_id":%q,"accepted":true}`, identity().NodeID)
	}))
	defer srv.Close()
	id := identity()
	id.CoreURL = srv.URL
	s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Reconnecting}, id: id}
	m := NewMonitor()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { Run(ctx, srv.URL, "test", s, 5*time.Second, m); close(done) }()
	first := <-requests
	start := time.Now()
	select {
	case next := <-requests:
		a, _ := strconv.ParseInt(first.Header.Get("X-ArkNet-Node-Timestamp"), 10, 64)
		b, _ := strconv.ParseInt(next.Header.Get("X-ArkNet-Node-Timestamp"), 10, 64)
		if b <= a || time.Since(start) < 4900*time.Millisecond {
			t.Fatal("non-increasing proof or aggressive loop")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("no next regular heartbeat")
	}
	cancel()
	<-done
	if m.Connectivity() != "CONNECTED" {
		t.Fatal(m.Connectivity())
	}
}
func TestIdentityGatesAndCASConflict(t *testing.T) {
	id := identity()
	id.CoreURL = "http://127.0.0.1:1"
	for _, mutate := range []func(*memoryStore){func(s *memoryStore) { s.id.Status = "PENDING" }, func(s *memoryStore) { s.id.PrivateSeed = nil }, func(s *memoryStore) { s.id.CoreURL = "http://other" }, func(s *memoryStore) { s.snap.NodeID = "wrong" }, func(s *memoryStore) { s.conflict = true }} {
		s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Offline}, id: id}
		mutate(s)
		w := newWorker(s, id.CoreURL, "test", 30*time.Second, NewMonitor())
		if w.attempt(context.Background()) || w.failures != 0 || len(s.history) != 0 {
			t.Fatal("ineligible identity or failed CAS attempted network")
		}
	}
}
func TestRegularSuccessPreservesAdvancedStates(t *testing.T) {
	for _, state := range []localstate.State{localstate.Synchronizing, localstate.Mesh} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"node_id":%q,"accepted":true}`, identity().NodeID)
		}))
		id := identity()
		id.CoreURL = srv.URL
		s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: state}, id: id}
		w := newWorker(s, srv.URL, "test", 30*time.Second, NewMonitor())
		w.attempt(context.Background())
		srv.Close()
		if s.snap.State != state || len(s.history) != 0 || w.monitor.Connectivity() != "CONNECTED" {
			t.Fatal("regressed advanced state")
		}
	}
}
func TestMonitorConcurrentReads(t *testing.T) {
	m := NewMonitor()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				m.set("RECONNECTING")
				_ = m.Connectivity()
			}
		}()
	}
	wg.Wait()
}
func TestUnhealthyDatabaseReportsRealFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Health map[string]bool `json:"health"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		if b.Health["healthy"] || b.Health["database_healthy"] {
			t.Error("fake health")
		}
		fmt.Fprintf(w, `{"node_id":%q,"accepted":true}`, identity().NodeID)
	}))
	defer srv.Close()
	id := identity()
	id.CoreURL = srv.URL
	s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Connected}, id: id, pingErr: fmt.Errorf("SECRET")}
	newWorker(s, srv.URL, "test", 30*time.Second, nil).attempt(context.Background())
	if !s.pingBound {
		t.Error("unbounded Ping")
	}
}
