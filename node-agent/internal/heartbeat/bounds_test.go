package heartbeat

import (
	"arknet/node-agent/internal/localstate"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRejectUnsafeACKs(t *testing.T) {
	id := identity()
	for _, ack := range []string{`{}`, `null`, `[]`, `{"node_id":"wrong","accepted":true}`, fmt.Sprintf(`{"node_id":%q,"accepted":"true"}`, id.NodeID), fmt.Sprintf(`{"node_id":%q,"accepted":false}`, id.NodeID), fmt.Sprintf(`{"node_id":%q,"accepted":true,"extra":true}`, id.NodeID), fmt.Sprintf(`{"node_id":%q,"accepted":true,"accepted":true}`, id.NodeID), fmt.Sprintf(`{"node_id":%q,"node_id":%q,"accepted":true}`, id.NodeID, id.NodeID), fmt.Sprintf(`{"node_id":%q,"accepted":true} garbage`, id.NodeID), fmt.Sprintf(`{"node_id":%q,"accepted":true}{}`, id.NodeID), strings.Repeat(" ", 4097)} {
		t.Run(ack[:min(len(ack), 24)], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, ack) }))
			defer srv.Close()
			id.CoreURL = srv.URL
			if err := send(context.Background(), id, "test", localstate.Connected, true, false); err != ErrRequestFailed {
				t.Fatalf("accepted unsafe ACK: %v", err)
			}
		})
	}
}
func TestTransportRejectsRedirectAuthFailureAndUntrustedTLS(t *testing.T) {
	id := identity()
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	for _, code := range []int{301, 302, 307, 401, 403, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", target.URL)
			w.WriteHeader(code)
		}))
		id.CoreURL = srv.URL
		want := ErrRequestFailed
		if code == http.StatusForbidden {
			want = ErrNotApproved // rejected; revocation still needs signed confirmation
		}
		if send(context.Background(), id, "test", localstate.Connected, true, false) != want {
			t.Errorf("accepted %d", code)
		}
		srv.Close()
	}
	if forwarded.Load() != 0 {
		t.Fatal("followed redirect")
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS reached handler") }))
	defer tls.Close()
	id.CoreURL = tls.URL
	if send(context.Background(), id, "test", localstate.Connected, true, false) != ErrRequestFailed {
		t.Fatal("untrusted TLS accepted")
	}
}
func TestReadTimeoutAndCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelEarly), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				close(entered)
				<-release
			}))
			defer srv.Close()
			defer close(release)
			id := identity()
			id.CoreURL = srv.URL
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			started := time.Now()
			go func() { done <- send(ctx, id, "test", localstate.Connected, true, false) }()
			<-entered
			if cancelEarly {
				cancel()
			}
			select {
			case err := <-done:
				if err != ErrRequestFailed {
					t.Fatal(err)
				}
				if !cancelEarly && time.Since(started) < 4*time.Second {
					t.Fatal("timeout too early")
				}
			case <-time.After(6 * time.Second):
				t.Fatal("read not bounded")
			}
		})
	}
}
