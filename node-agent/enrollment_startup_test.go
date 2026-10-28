package main

import (
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type startupStore struct{ records int }

func (s *startupStore) Snapshot(context.Context) (localstate.Snapshot, error) {
	return localstate.Snapshot{NodeID: "node_" + strings.Repeat("a", 64), State: localstate.Provisioning}, nil
}
func (s *startupStore) RecordEnrollmentPending(context.Context, string) (localstate.Snapshot, error) {
	s.records++
	return localstate.Snapshot{}, nil
}
func (s *startupStore) EnrollmentIdentity(context.Context) (enrollment.Identity, error) {
	return enrollment.Identity{NodeID: "node_" + strings.Repeat("a", 64), Status: "NOT_ENROLLED", PrivateSeed: make([]byte, 32)}, nil
}
func (s *startupStore) CompleteEnrollment(context.Context, string, string) (localstate.Snapshot, error) {
	return localstate.Snapshot{}, nil
}
func TestStartupEnrollmentFailureSanitized(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "SECRET", 500) }))
	defer server.Close()
	var logs bytes.Buffer
	store := &startupStore{}
	enrollAtStartup(context.Background(), server.URL, store, slog.New(slog.NewJSONHandler(&logs, nil)))
	if calls != 1 || store.records != 0 {
		t.Fatal("startup enrollment retried or changed state")
	}
	if !strings.Contains(logs.String(), "enrollment_request_failed") || strings.Contains(logs.String(), server.URL) || strings.Contains(logs.String(), "SECRET") {
		t.Fatalf("unsafe/missing log %s", logs.String())
	}
}
func TestStartupEnrollmentDisabled(t *testing.T) {
	var logs bytes.Buffer
	enrollAtStartup(context.Background(), "", nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	if logs.Len() != 0 {
		t.Fatal("disabled enrollment logged")
	}
}
