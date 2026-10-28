package postgres

import (
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnrollmentAckAtomicAndIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	initial, _ := s.Snapshot(ctx)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"node_id": initial.NodeID, "enrollment_status": "PENDING"})
	}))
	defer server.Close()
	if err := enrollment.Submit(ctx, server.URL, "test", s); err != nil {
		t.Fatal(err)
	}
	first, err := s.Snapshot(ctx)
	if err != nil || first.State != localstate.EnrollmentPending || first.NodeID != initial.NodeID {
		t.Fatalf("snapshot %+v %v", first, err)
	}
	if err := enrollment.Submit(ctx, server.URL, "test", s); err != nil {
		t.Fatal(err)
	}
	second, _ := s.Snapshot(ctx)
	if second != first {
		t.Fatal("repeat changed timestamp or identity")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM node_state_transitions").Scan(&count); err != nil || count != 1 {
		t.Fatalf("history %d %v", count, err)
	}
	var status, url string
	var enrolled sql.NullTime
	if err := s.db.QueryRowContext(ctx, "SELECT status,core_url,enrolled_at FROM core_enrollment").Scan(&status, &url, &enrolled); err != nil || status != "PENDING" || url != server.URL || enrolled.Valid {
		t.Fatalf("enrollment %s %s %v %v", status, url, enrolled, err)
	}
	if _, err := s.RecordEnrollmentPending(ctx, "https://different.example"); err == nil {
		t.Fatal("bound Core changed")
	}
	after, _ := s.Snapshot(ctx)
	if after != first {
		t.Fatal("binding rejection modified state")
	}
}
