package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type startupLogCapture struct {
	mu      sync.Mutex
	logs    strings.Builder
	started chan string
}

func (w *startupLogCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.logs.Write(p)
	var event map[string]any
	if json.Unmarshal(p, &event) == nil && event["msg"] == "server_started" {
		w.started <- event["addr"].(string)
	}
	return len(p), nil
}
func (w *startupLogCapture) text() string { w.mu.Lock(); defer w.mu.Unlock(); return w.logs.String() }
func startupTestConfig(t *testing.T) config {
	t.Helper()
	dsn := os.Getenv("ARKNET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ARKNET_TEST_DATABASE_URL for startup integration")
	}
	ctx := context.Background()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "agent_startup_" + hex.EncodeToString(b)
	if _, err := db.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		db.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
		db.Close(ctx)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return config{addr: "127.0.0.1:0", databaseURL: u.String(), shutdownTimeout: time.Second}
}
func TestRunLocalAPIStartsWhileCoreBlockedAndWorkerJoins(t *testing.T) {
	cfg := startupTestConfig(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	defer server.Close()
	defer close(release)
	cfg.coreURL = server.URL
	logs := &startupLogCapture{started: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil))) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	var addr string
	select {
	case addr = <-logs.started:
	case <-time.After(time.Second):
		t.Fatal("local API blocked on Core")
	}
	response, err := http.Get("http://" + addr + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker not canceled/joined")
	}
}

var _ io.Writer = (*startupLogCapture)(nil)
