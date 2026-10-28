package main

import (
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/heartbeat"
	"arknet/node-agent/internal/localstate"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStateTransitionLoggerUsesOnlyStateFields(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	stateTransitionLogger(logger)(localstate.Connected, localstate.Degraded)

	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event["msg"] != "node_state_transition" || event["from_state"] != "CONNECTED" || event["to_state"] != "DEGRADED" {
		t.Fatalf("unexpected event: %v", event)
	}
	for key := range event {
		switch key {
		case "time", "level", "msg", "from_state", "to_state":
		default:
			t.Fatalf("unexpected log field %q", key)
		}
	}
}

func TestHandlerHeartbeatMonitor(t *testing.T) {
	m := heartbeat.NewMonitor()
	w := httptest.NewRecorder()
	newHandler(testRepository{}, monitorOpt{m}).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/status", nil))
	var got nodeStatus
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.CoreConnectivity != "UNKNOWN" {
		t.Fatal(got.CoreConnectivity)
	}
}

type approvedStore struct {
	startupStore
	coreURL string
	state   localstate.State
}

type transitionLogCapture struct {
	events chan map[string]any
}

func (c *transitionLogCapture) Write(p []byte) (int, error) {
	var event map[string]any
	if json.Unmarshal(p, &event) == nil && event["msg"] == "node_state_transition" {
		c.events <- event
	}
	return len(p), nil
}

func (s *approvedStore) Snapshot(context.Context) (localstate.Snapshot, error) {
	return localstate.Snapshot{NodeID: "node_" + strings.Repeat("a", 64), State: s.state}, nil
}
func (s *approvedStore) EnrollmentIdentity(context.Context) (enrollment.Identity, error) {
	return enrollment.Identity{NodeID: "node_" + strings.Repeat("a", 64), PrivateSeed: make([]byte, 32), Status: "APPROVED", CoreURL: s.coreURL}, nil
}
func (s *approvedStore) Ping(context.Context) error                { return nil }
func (s *approvedStore) PeerServing(context.Context) (bool, error) { return false, nil }
func (s *approvedStore) RecordSuccessfulCoreContact(context.Context) (time.Time, error) {
	return time.Now(), nil
}
func (s *approvedStore) Transition(ctx context.Context, from, to localstate.State) (localstate.Snapshot, error) {
	if from != s.state {
		return localstate.Snapshot{}, localstate.ErrStateConflict
	}
	if err := localstate.Validate(from, to); err != nil {
		return localstate.Snapshot{}, err
	}
	s.state = to
	return s.Snapshot(ctx)
}

func TestFederationWorkerWiresStateTransitionLogger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	logs := &transitionLogCapture{events: make(chan map[string]any, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	s := &approvedStore{coreURL: srv.URL, state: localstate.Connected}
	done := startFederationWorker(ctx, config{coreURL: srv.URL, heartbeatInterval: 5 * time.Second}, s, slog.New(slog.NewJSONHandler(logs, nil)), heartbeat.NewMonitor())
	defer func() {
		cancel()
		<-done
	}()

	select {
	case event := <-logs.events:
		if event["from_state"] != "CONNECTED" || event["to_state"] != "DEGRADED" {
			t.Fatalf("unexpected event: %v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("state transition was not logged")
	}
}

func TestFederationWorkerApprovedRebootImmediateAndJoined(t *testing.T) {
	for _, state := range []localstate.State{localstate.Connected, localstate.Degraded, localstate.Offline, localstate.Reconnecting} {
		t.Run(string(state), func(t *testing.T) {
			entered := make(chan string, 1)
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- r.URL.Path; <-release; fmt.Fprint(w, `{}`) }))
			defer srv.Close()
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &approvedStore{coreURL: srv.URL, state: state}
			done := startFederationWorker(ctx, config{coreURL: srv.URL, heartbeatInterval: 5 * time.Second}, s, slog.New(slog.NewTextHandler(io.Discard, nil)), heartbeat.NewMonitor())
			select {
			case path := <-entered:
				if !strings.HasSuffix(path, "/heartbeats") {
					t.Fatalf("reenrollment %s", path)
				}
			case <-time.After(time.Second):
				t.Fatal("no immediate heartbeat")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("did not join")
			}
		})
	}
}
