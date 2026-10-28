package heartbeat

import (
	"arknet/node-agent/internal/localstate"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// revocationCore refuses heartbeats as an authenticated-but-unapproved Node
// and answers the signed enrollment-status probe with the given decision.
func revocationCore(t *testing.T, decision string, statusCode int) (*httptest.Server, *int) {
	t.Helper()
	probes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeats"):
			http.Error(w, "node is not approved", http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/enrollment") && r.Method == http.MethodGet:
			probes++
			if r.Header.Get("X-ArkNet-Node-Signature") == "" {
				t.Error("enrollment status probe was not signed")
			}
			if statusCode != http.StatusOK {
				w.WriteHeader(statusCode)
				return
			}
			parts := strings.SplitN(decision, "/", 2)
			fmt.Fprintf(w, `{"node_id":%q,"enrollment_status":%q,"trust_status":%q}`, identity().NodeID, parts[0], parts[1])
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &probes
}

func TestAuthoritativeRevocationMovesNodeToTerminalRevoked(t *testing.T) {
	srv, probes := revocationCore(t, "REVOKED/BLOCKED", http.StatusOK)
	id := identity()
	id.CoreURL = srv.URL
	s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Connected}, id: id}
	w := newWorker(s, srv.URL, "test", 30*time.Second, nil)
	w.attempt(context.Background())
	if s.snap.State != localstate.Revoked || *probes != 1 {
		t.Fatalf("state=%s probes=%d history=%v", s.snap.State, *probes, s.history)
	}
	// Terminal: no further heartbeats are attempted.
	if w.attempt(context.Background()) {
		t.Fatal("revoked Node attempted another heartbeat")
	}
}

func TestRevocationIsAlsoDetectedFromOfflineReconnect(t *testing.T) {
	srv, _ := revocationCore(t, "REVOKED/BLOCKED", http.StatusOK)
	id := identity()
	id.CoreURL = srv.URL
	s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Offline}, id: id}
	newWorker(s, srv.URL, "test", 30*time.Second, nil).attempt(context.Background())
	if s.snap.State != localstate.Revoked {
		t.Fatalf("state=%s history=%v", s.snap.State, s.history)
	}
}

func TestForbiddenHeartbeatWithoutConfirmedRevocationIsOnlyAFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		decision string
		status   int
	}{
		"still approved":   {"APPROVED/TRUSTED", http.StatusOK},
		"status error":     {"", http.StatusServiceUnavailable},
		"rejected pending": {"PENDING/UNTRUSTED", http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := revocationCore(t, tc.decision, tc.status)
			id := identity()
			id.CoreURL = srv.URL
			s := &memoryStore{snap: localstate.Snapshot{NodeID: id.NodeID, State: localstate.Connected}, id: id}
			newWorker(s, srv.URL, "test", 30*time.Second, nil).attempt(context.Background())
			if s.snap.State != localstate.Degraded {
				t.Fatalf("state=%s, want DEGRADED", s.snap.State)
			}
		})
	}
}
