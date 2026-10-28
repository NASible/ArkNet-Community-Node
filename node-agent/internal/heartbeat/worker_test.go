package heartbeat

import (
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memoryStore struct {
	snap        localstate.Snapshot
	id          enrollment.Identity
	history     []localstate.State
	pingErr     error
	conflict    bool
	pingBound   bool
	contacts    int
	contactErr  error
	peerServing bool
}

func (s *memoryStore) Snapshot(context.Context) (localstate.Snapshot, error) { return s.snap, nil }
func (s *memoryStore) EnrollmentIdentity(context.Context) (enrollment.Identity, error) {
	return s.id, nil
}
func (s *memoryStore) Ping(ctx context.Context) error {
	d, ok := ctx.Deadline()
	s.pingBound = ok && time.Until(d) <= 2*time.Second
	return s.pingErr
}
func (s *memoryStore) RecordSuccessfulCoreContact(context.Context) (time.Time, error) {
	s.contacts++
	return time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC), s.contactErr
}
func (s *memoryStore) PeerServing(context.Context) (bool, error) { return s.peerServing, nil }

func TestHeartbeatReportsPeerServingParticipation(t *testing.T) {
	var reported bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Capabilities struct {
				PeerServing bool `json:"peer_serving"`
			} `json:"capabilities"`
		}
		decodeBody(t, r, &body)
		reported = body.Capabilities.PeerServing
		fmt.Fprintf(w, `{"node_id":%q,"accepted":true}`, identity().NodeID)
	}))
	defer srv.Close()
	id := identity()
	id.CoreURL = srv.URL
	s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Connected}, id: id, peerServing: true}
	newWorker(s, srv.URL, "test", 30*time.Second, nil).attempt(context.Background())
	if !reported {
		t.Fatal("peer serving opt-in was not reported")
	}
}

func (s *memoryStore) Transition(ctx context.Context, expected, next localstate.State) (localstate.Snapshot, error) {
	if s.conflict || expected != s.snap.State {
		return s.snap, localstate.ErrStateConflict
	}
	if err := localstate.Validate(expected, next); err != nil {
		return s.snap, err
	}
	s.snap.State = next
	s.snap.StateChangedAt = time.Now()
	s.history = append(s.history, next)
	return s.snap, nil
}

func TestSuccessfulTransitionNotifiesObserver(t *testing.T) {
	s := &memoryStore{snap: localstate.Snapshot{State: localstate.Connected}}
	var gotFrom, gotTo localstate.State
	calls := 0
	w := newWorker(s, "http://127.0.0.1:1", "test", 30*time.Second, nil, func(from, to localstate.State) {
		calls++
		gotFrom, gotTo = from, to
	})

	if !w.transition(context.Background(), localstate.Connected, localstate.Degraded) {
		t.Fatal("transition failed")
	}
	if calls != 1 || gotFrom != localstate.Connected || gotTo != localstate.Degraded {
		t.Fatalf("calls=%d transition=%s->%s", calls, gotFrom, gotTo)
	}
}

func TestFailedCASTransitionDoesNotNotifyObserver(t *testing.T) {
	s := &memoryStore{snap: localstate.Snapshot{State: localstate.Connected}, conflict: true}
	calls := 0
	w := newWorker(s, "http://127.0.0.1:1", "test", 30*time.Second, nil, func(_, _ localstate.State) {
		calls++
	})

	if w.transition(context.Background(), localstate.Connected, localstate.Degraded) {
		t.Fatal("transition unexpectedly succeeded")
	}
	if calls != 0 {
		t.Fatalf("observer called %d times", calls)
	}
}

func TestFailureRecovery(t *testing.T) {
	attempts := 0
	var states []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var b struct {
			State string `json:"state"`
		}
		decodeBody(t, r, &b)
		states = append(states, b.State)
		if attempts <= 3 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(w, `{"node_id":%q,"accepted":true}`, identity().NodeID)
	}))
	defer srv.Close()
	id := identity()
	id.CoreURL = srv.URL
	s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Connected}, id: id}
	worker := newWorker(s, srv.URL, "test", 30*time.Second, NewMonitor())
	for i, want := range []localstate.State{localstate.Degraded, localstate.Degraded, localstate.Offline, localstate.Connected, localstate.Connected} {
		worker.attempt(context.Background())
		if s.snap.State != want {
			t.Fatalf("attempt %d state %s", i, s.snap.State)
		}
	}
	if strings.Join(states, ",") != "CONNECTED,RECONNECTING,RECONNECTING,RECONNECTING,CONNECTED" {
		t.Fatal(states)
	}
	if worker.failures != 0 || !s.pingBound || len(s.history) != 7 {
		t.Fatalf("failures=%d history=%v bound=%v", worker.failures, s.history, s.pingBound)
	}
}
func TestPersistedOfflineDoesNotRegressDuringContinuingOutage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	id := identity()
	id.CoreURL = srv.URL
	s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Offline}, id: id}
	worker := newWorker(s, srv.URL, "test", 30*time.Second, NewMonitor())

	worker.attempt(context.Background())

	if s.snap.State != localstate.Offline || worker.failures < 3 {
		t.Fatalf("state=%s failures=%d", s.snap.State, worker.failures)
	}
}

func TestAcceptedHeartbeatRecordsSuccessfulContact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"node_id":%q,"accepted":true}`, identity().NodeID)
	}))
	defer srv.Close()
	id := identity()
	id.CoreURL = srv.URL
	s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Connected}, id: id}
	w := newWorker(s, srv.URL, "test", 30*time.Second, NewMonitor())

	w.attempt(context.Background())

	if s.contacts != 1 {
		t.Fatalf("successful contacts = %d", s.contacts)
	}
}

func TestAcceptedHeartbeatRunsRefreshHookButFailureDoesNot(t *testing.T) {
	accepted := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !accepted {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"node_id":%q,"accepted":true}`, identity().NodeID)
	}))
	defer srv.Close()
	id := identity()
	id.CoreURL = srv.URL
	s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Connected}, id: id}
	calls := 0
	w := newWorkerWithSuccess(s, srv.URL, "test", 30*time.Second, NewMonitor(), func(_ context.Context, got enrollment.Identity) error {
		calls++
		if got.NodeID != id.NodeID {
			t.Fatal("wrong identity")
		}
		return nil
	})
	w.attempt(context.Background())
	accepted = false
	w.attempt(context.Background())
	if calls != 1 {
		t.Fatalf("refresh hook calls = %d", calls)
	}
}

func TestRejectedHeartbeatDoesNotRecordSuccessfulContact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	id := identity()
	id.CoreURL = srv.URL
	s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Connected}, id: id}
	w := newWorker(s, srv.URL, "test", 30*time.Second, NewMonitor())

	w.attempt(context.Background())

	if s.contacts != 0 {
		t.Fatalf("successful contacts = %d", s.contacts)
	}
}

func TestIneligibleNeverSends(t *testing.T) {
	for _, state := range []localstate.State{localstate.Provisioning, localstate.EnrollmentPending, localstate.Quarantined, localstate.Revoked, localstate.Error} {
		s := &memoryStore{snap: localstate.Snapshot{State: state}, id: identity()}
		w := newWorker(s, "http://127.0.0.1:1", "test", 30*time.Second, nil)
		if w.attempt(context.Background()) {
			t.Fatalf("active %s", state)
		}
		if len(s.history) != 0 {
			t.Fatal(s.history)
		}
	}
}
func TestBackoffBounds(t *testing.T) {
	for i, seconds := range []int{30, 60, 120, 240, 300, 300} {
		for _, j := range []float64{0, .5, 1} {
			got := backoff(30*time.Second, i+1, j)
			base := time.Duration(seconds) * time.Second
			if got < base || got > base+base/10 || got > 5*time.Minute {
				t.Fatalf("%d %v", i, got)
			}
		}
	}
	if backoff(5*time.Second, 1000, 1) != 5*time.Minute {
		t.Fatal("cap")
	}
}
func TestRunCancellationAndInvalidInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &memoryStore{snap: localstate.Snapshot{State: localstate.Quarantined}, id: identity()}
	done := make(chan struct{})
	go func() { Run(ctx, "http://127.0.0.1:1", "test", s, 0, nil); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("not joined")
	}
}
func decodeBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Error(err)
	}
}
