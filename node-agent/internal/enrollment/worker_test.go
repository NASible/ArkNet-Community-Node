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

func TestWorkerApprovalAfterConflictingSubmitAndStops(t *testing.T) {
	id := "node_" + strings.Repeat("a", 64)
	posts, gets := 0, 0
	store := &memoryStore{snapshot: localstate.Snapshot{NodeID: id, State: localstate.EnrollmentPending}, status: "PENDING"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
			w.WriteHeader(409)
			return
		}
		gets++
		status, trust := "PENDING", "UNTRUSTED"
		if gets == 2 {
			status, trust = "APPROVED", "TRUSTED"
		}
		json.NewEncoder(w).Encode(map[string]string{"node_id": id, "enrollment_status": status, "trust_status": trust})
	}))
	defer srv.Close()
	store.url = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	run(ctx, srv.URL, "test", store, nil, time.Millisecond)
	if posts != 1 || gets != 2 || store.completed != 1 {
		t.Fatalf("post=%d get=%d completed=%d", posts, gets, store.completed)
	}
	run(ctx, srv.URL, "test", store, nil, time.Millisecond)
	if posts != 1 || gets != 2 {
		t.Fatal("restart made network request")
	}
}
func TestWorkerRejectedStopsWithoutConnecting(t *testing.T) {
	id := "node_" + strings.Repeat("a", 64)
	gets := 0
	store := &memoryStore{snapshot: localstate.Snapshot{NodeID: id, State: localstate.EnrollmentPending}, status: "PENDING"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(409)
			return
		}
		gets++
		json.NewEncoder(w).Encode(map[string]string{"node_id": id, "enrollment_status": "REJECTED", "trust_status": "BLOCKED"})
	}))
	defer srv.Close()
	store.url = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	run(ctx, srv.URL, "test", store, nil, time.Millisecond)
	if gets != 1 || store.completed != 0 {
		t.Fatal("rejection did not stop")
	}
}
