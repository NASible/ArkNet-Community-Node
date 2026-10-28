package main

import (
	"arknet/node-agent/internal/localstate"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunLearnsApprovalAndRebootDoesNotReenroll(t *testing.T) {
	cfg := startupTestConfig(t)
	var posts, gets, heartbeats atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/media-misses") {
			// Media-miss sync worker; not an enrollment request.
			json.NewEncoder(w).Encode(map[string]any{"misses": []any{}})
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/media") {
			// Removal reconciliation and miss sync catalog reads; not enrollment.
			json.NewEncoder(w).Encode(map[string]any{"objects": []any{}, "page": 1, "limit": 100})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/heartbeats") {
			heartbeats.Add(1)
			parts := strings.Split(r.URL.Path, "/")
			json.NewEncoder(w).Encode(map[string]any{"node_id": parts[len(parts)-2], "accepted": true})
			return
		}
		if r.Method == "POST" {
			posts.Add(1)
			var body struct {
				NodeID string `json:"node_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]string{"node_id": body.NodeID, "enrollment_status": "PENDING"})
			return
		}
		gets.Add(1)
		parts := strings.Split(r.URL.Path, "/")
		json.NewEncoder(w).Encode(map[string]string{"node_id": parts[len(parts)-2], "enrollment_status": "APPROVED", "trust_status": "TRUSTED"})
	}))
	defer core.Close()
	cfg.coreURL = core.URL
	for range 2 {
		logs := &startupLogCapture{started: make(chan string, 1)}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- run(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil))) }()
		var addr string
		select {
		case addr = <-logs.started:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("startup timed out")
		}
		deadline := time.Now().Add(3 * time.Second)
		var status nodeStatus
		client := &http.Client{Timeout: time.Second}
		for {
			response, err := client.Get("http://" + addr + "/api/v1/status")
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			err = json.NewDecoder(response.Body).Decode(&status)
			response.Body.Close()
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			if status.State == localstate.Connected && status.CoreConnectivity == "CONNECTED" {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("approval not learned: %s", status.State)
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown timed out")
		}
	}
	if heartbeats.Load() != 2 {
		t.Fatalf("expected immediate heartbeat on both starts: %d", heartbeats.Load())
	}
	if posts.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("unexpected restart requests POST=%d GET=%d", posts.Load(), gets.Load())
	}
}
