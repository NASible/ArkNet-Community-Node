package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// The startup log must match /health and /version: this binary always runs
// the real libtorrent worker, never the in-memory mock engine.
func TestServerStartedLogReportsRealBackend(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = serve(ctx, newServer(http.NotFoundHandler(), testLogger()), listener, time.Second,
		slog.New(slog.NewJSONHandler(&buffer, nil)))
	var started map[string]any
	for _, line := range bytes.Split(buffer.Bytes(), []byte("\n")) {
		var entry map[string]any
		if json.Unmarshal(line, &entry) == nil && entry["msg"] == "server_started" {
			started = entry
		}
	}
	if started == nil {
		t.Fatalf("no server_started log line in %q", buffer.String())
	}
	if started["mock"] != false || started["backend"] != "libtorrent" {
		t.Fatalf("server_started reports mock=%v backend=%v", started["mock"], started["backend"])
	}
}
func TestServerTimeouts(t *testing.T) {
	s := newServer(http.NotFoundHandler(), testLogger())
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.IdleTimeout <= 0 || s.MaxHeaderBytes <= 0 {
		t.Fatal("unbounded HTTP server")
	}
}
func TestServeDrainsAndDeadline(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "drains", true: "deadline"}[timeout], func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			s := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.Write([]byte("ok")) }), testLogger())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			duration := 2 * time.Second
			if timeout {
				duration = 25 * time.Millisecond
			}
			done := make(chan error, 1)
			go func() { done <- serve(ctx, s, listener, duration, testLogger()) }()
			clientDone := make(chan error, 1)
			go func() {
				client := &http.Client{Timeout: 3 * time.Second}
				resp, err := client.Get("http://" + listener.Addr().String())
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				clientDone <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("request not entered")
			}
			cancel()
			if timeout {
				select {
				case err := <-done:
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("shutdown hung")
				}
				select {
				case err := <-clientDone:
					if err == nil {
						t.Fatal("connection not closed")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("client hung")
				}
			} else {
				select {
				case err := <-done:
					t.Fatalf("returned before drain: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
				release <- struct{}{}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("drain hung")
				}
				if err := <-clientDone; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestListenFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := run(context.Background(), config{addr: l.Addr().String(), token: testToken}, testLogger()); err == nil {
		t.Fatal("expected occupied port failure")
	}
}
