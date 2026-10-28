package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStartEnrollmentWorkerReturnsImmediatelyAndJoins(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := startEnrollmentWorker(ctx, srv.URL, &startupStore{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	cancel()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("worker did not join")
	}
}
