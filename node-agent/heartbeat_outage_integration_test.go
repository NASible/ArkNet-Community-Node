package main

import (
	"arknet/node-agent/internal/localstate"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunCoreOutageKeepsAPIAndAutomaticallyRecovers(t *testing.T) {
	cfg := startupTestConfig(t)
	cfg.heartbeatInterval = 5 * time.Second
	var recovered atomic.Bool
	proofs := make(chan localstate.State, 4)
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeats") {
			var body struct {
				NodeID string           `json:"node_id"`
				State  localstate.State `json:"state"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			proofs <- body.State
			if !recovered.Load() {
				http.Error(w, "SECRET", 503)
				return
			}
			fmt.Fprintf(w, `{"node_id":%q,"accepted":true}`, body.NodeID)
			return
		}
		if r.Method == "POST" {
			var body struct {
				NodeID string `json:"node_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]string{"node_id": body.NodeID, "enrollment_status": "PENDING"})
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		json.NewEncoder(w).Encode(map[string]string{"node_id": parts[len(parts)-2], "enrollment_status": "APPROVED", "trust_status": "TRUSTED"})
	}))
	defer core.Close()
	cfg.coreURL = core.URL
	logs := &startupLogCapture{started: make(chan startupAddrs, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil))) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("worker did not join before database close")
		}
	}()
	var addr startupAddrs
	select {
	case addr = <-logs.started:
	case <-time.After(5 * time.Second):
		t.Fatal("API not started")
	}
	cookie := startupOperatorCookie(t, cfg, "http://"+addr.member)
	readStatus := func() nodeStatus {
		t.Helper()
		return startupStatus(t, "http://"+addr.management, cookie)
	}
	waitStatus := func(want localstate.State) nodeStatus {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			status := readStatus()
			if status.State == want && status.CoreConnectivity == string(want) {
				return status
			}
			select {
			case err := <-done:
				done <- err
				t.Fatalf("Core failure terminated API: %v", err)
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("never reached %s", want)
		return nodeStatus{}
	}
	degraded := waitStatus(localstate.Degraded)
	if degraded.StateChangedAt.IsZero() {
		t.Fatal("transition timestamp not persisted")
	}
	recovered.Store(true)
	connected := waitStatus(localstate.Connected)
	if !connected.StateChangedAt.After(degraded.StateChangedAt) {
		t.Fatal("recovery timestamp not persisted")
	}
	for i, want := range []localstate.State{localstate.Connected, localstate.Reconnecting, localstate.Connected} {
		select {
		case got := <-proofs:
			if got != want {
				t.Fatalf("proof %d: %s", i, got)
			}
		case <-time.After(7 * time.Second):
			t.Fatal("no next regular proof")
		}
	}
}
