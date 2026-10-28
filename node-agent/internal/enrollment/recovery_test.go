package enrollment

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/localstate"
)

// Core may commit registration and approve before the lost receipt reaches us.
// A public ID is insufficient: recovery still uses this node's saved signing key.
func TestRunReconcilesApprovedRegistrationAfterLostReceipt(t *testing.T) {
	id := "node_" + strings.Repeat("a", 64)
	store := &memoryStore{snapshot: localstate.Snapshot{NodeID: id, State: localstate.Provisioning}}
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, "already decided", http.StatusConflict)
			return
		}
		gets++
		if r.Header.Get("X-ArkNet-Node-Signature") == "" {
			t.Error("unsigned recovery")
		}
		w.Write([]byte(`{"node_id":"` + id + `","enrollment_status":"APPROVED","trust_status":"TRUSTED"}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	run(ctx, srv.URL, "test", store, nil, time.Millisecond)
	if gets != 1 || store.records != 1 || store.completed != 1 || store.snapshot.State != localstate.Connected || store.url != srv.URL {
		t.Fatalf("lost-receipt recovery did not complete: gets=%d pending=%d complete=%d state=%s", gets, store.records, store.completed, store.snapshot.State)
	}
}
