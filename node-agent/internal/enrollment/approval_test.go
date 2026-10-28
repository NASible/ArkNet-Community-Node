package enrollment

import (
	"arknet/node-agent/internal/localstate"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func (s *memoryStore) EnrollmentIdentity(context.Context) (Identity, error) {
	id := s.snapshot.NodeID
	if id == "" {
		id = "node_" + strings.Repeat("a", 64)
	}
	status := s.status
	if status == "" {
		status = "NOT_ENROLLED"
	}
	return Identity{NodeID: id, CoreURL: s.url, Status: status, PrivateSeed: make([]byte, 32)}, nil
}
func (s *memoryStore) CompleteEnrollment(_ context.Context, url, id string) (localstate.Snapshot, error) {
	s.completed++
	s.status = "APPROVED"
	s.snapshot.State = localstate.Connected
	return s.snapshot, nil
}
func TestCheckApprovalStrictPairs(t *testing.T) {
	id := "node_" + strings.Repeat("a", 64)
	for _, test := range []struct {
		body         string
		ok, complete bool
	}{
		{`{"node_id":"` + id + `","enrollment_status":"PENDING","trust_status":"UNTRUSTED"}`, true, false},
		{`{"node_id":"` + id + `","enrollment_status":"APPROVED","trust_status":"TRUSTED"}`, true, true},
		{`{"node_id":"` + id + `","enrollment_status":"REJECTED","trust_status":"BLOCKED"}`, true, false},
		{`{"node_id":"` + id + `","enrollment_status":"APPROVED","trust_status":"UNTRUSTED"}`, false, false},
		{`{"node_id":"wrong","enrollment_status":"APPROVED","trust_status":"TRUSTED"}`, false, false},
		{`{"node_id":"` + id + `","enrollment_status":"APPROVED","trust_status":"TRUSTED","x":"x"}`, false, false},
		{`{"node_id":"` + id + `","enrollment_status":"APPROVED","trust_status":"TRUSTED","trust_status":"TRUSTED"}`, false, false},
		{`{}` + strings.Repeat(" ", 4096), false, false},
	} {
		t.Run(test.body[:min(len(test.body), 100)], func(t *testing.T) {
			store := &memoryStore{snapshot: localstate.Snapshot{NodeID: id, State: localstate.EnrollmentPending}, status: "PENDING"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/federation/v1/nodes/"+id+"/enrollment" || r.Header.Get("X-ArkNet-Node-Signature") == "" {
					t.Error("invalid signed GET")
				}
				w.Write([]byte(test.body))
			}))
			defer srv.Close()
			store.url = srv.URL
			err := CheckApproval(context.Background(), srv.URL, store)
			if test.ok && err != nil || !test.ok && err != ErrRequestFailed {
				t.Fatalf("error %v", err)
			}
			if (store.completed == 1) != test.complete {
				t.Fatal("wrong completion")
			}
		})
	}
}
func TestCoreBindingAndCompletedSkipNetwork(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	store := &memoryStore{snapshot: localstate.Snapshot{NodeID: "node_" + strings.Repeat("a", 64), State: localstate.EnrollmentPending}, url: "https://bound.example", status: "PENDING"}
	if Submit(context.Background(), srv.URL, "test", store) != ErrRequestFailed || CheckApproval(context.Background(), srv.URL, store) != ErrRequestFailed {
		t.Fatal("URL rebinding accepted")
	}
	store.url = srv.URL
	store.status = "APPROVED"
	if Submit(context.Background(), srv.URL, "test", store) != nil || CheckApproval(context.Background(), srv.URL, store) != nil {
		t.Fatal("completed failed")
	}
	if calls != 0 {
		t.Fatal("unexpected network")
	}
}
