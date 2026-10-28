package enrollment

import (
	"arknet/node-agent/internal/localstate"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	snapshot  localstate.Snapshot
	records   int
	url       string
	status    string
	completed int
}

func (s *memoryStore) Snapshot(context.Context) (localstate.Snapshot, error) { return s.snapshot, nil }
func (s *memoryStore) RecordEnrollmentPending(_ context.Context, url string) (localstate.Snapshot, error) {
	s.records++
	s.url = url
	s.snapshot.State = localstate.EnrollmentPending
	return s.snapshot, nil
}
func TestSubmitRejectsInvalidAckWithoutMutation(t *testing.T) {
	id := "node_" + strings.Repeat("a", 64)
	for _, body := range []string{
		`null`, `{}`, `{"node_id":null,"enrollment_status":"PENDING"}`,
		`{"node_id":"` + id + `","enrollment_status":null}`,
		`{"node_id":"other","enrollment_status":"PENDING"}`,
		`{"node_id":"` + id + `","enrollment_status":"APPROVED"}`,
		`{"node_id":"` + id + `","enrollment_status":"PENDING","extra":true}`,
		`{"node_id":"` + id + `","enrollment_status":"PENDING"} {}`,
		`{"node_id":"` + id + `","enrollment_status":"PENDING"} rubbish`,
		`{"node_id":"` + id + `","enrollment_status":"PENDING"`,
		`{"node_id":"` + id + `","enrollment_status":"PENDING"}` + strings.Repeat(" ", 4096),
		`{"Node_ID":"` + id + `","enrollment_status":"PENDING"}`,
		`{"node_id":"` + id + `","enrollment_status":"PENDING","node_id":null}`,
		`{"node_id":"` + id + `","enrollment_status":"PENDING","node_id":"` + id + `"}`,
	} {
		t.Run(body[:min(len(body), 100)], func(t *testing.T) {
			store := &memoryStore{snapshot: localstate.Snapshot{NodeID: id, State: localstate.Provisioning}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer server.Close()
			if err := Submit(context.Background(), server.URL, "test", store); err != ErrRequestFailed {
				t.Fatalf("invalid ack accepted: %v", err)
			}
			if store.records != 0 || store.snapshot.State != localstate.Provisioning {
				t.Fatal("failed request mutated state")
			}
		})
	}
}

func TestSubmitExactRequestAndAck(t *testing.T) {
	store := &memoryStore{snapshot: localstate.Snapshot{NodeID: "node_" + strings.Repeat("a", 64), State: localstate.Provisioning}}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.String() != "/api/federation/v1/enrollments" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong request")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 4 || string(body["node_id"]) != `"`+store.snapshot.NodeID+`"` || string(body["version"]) != `"v-test"` || string(body["capabilities"]) != "{}" {
			t.Errorf("unexpected/PII-bearing payload: %s", body)
		}
		if store.records != calls-1 {
			t.Error("state advanced before ack")
		}
		json.NewEncoder(w).Encode(map[string]string{"node_id": store.snapshot.NodeID, "enrollment_status": "PENDING"})
	}))
	defer server.Close()
	for i := 0; i < 2; i++ {
		if err := Submit(context.Background(), server.URL, "v-test", store); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || store.records != 2 || store.url != server.URL || store.snapshot.State != localstate.EnrollmentPending {
		t.Fatal("missing pending records")
	}
}

func TestSubmitSkipsOtherStates(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	for _, state := range []localstate.State{localstate.Connected, localstate.Synchronizing, localstate.Mesh, localstate.Degraded, localstate.Offline, localstate.Reconnecting, localstate.Quarantined, localstate.Revoked, localstate.Error, "INVALID"} {
		store := &memoryStore{snapshot: localstate.Snapshot{State: state}}
		if err := Submit(context.Background(), server.URL, "test", store); err != nil {
			t.Fatal(err)
		}
		if store.records != 0 {
			t.Fatal("noneligible state mutated")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("noneligible state sent request")
	}
	if err := Submit(context.Background(), "", "test", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitRejectsRedirectsAndHTTPFailures(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	for _, status := range []int{201, 204, 301, 302, 303, 307, 308, 400, 401, 409, 500, 503} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", destination.URL)
			w.WriteHeader(status)
		}))
		store := &memoryStore{snapshot: localstate.Snapshot{State: localstate.Provisioning}}
		if err := Submit(context.Background(), server.URL, "test", store); err != ErrRequestFailed {
			t.Errorf("status %d error %v", status, err)
		}
		server.Close()
		if store.records != 0 {
			t.Fatal("failed request mutated state")
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestSubmitCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &memoryStore{snapshot: localstate.Snapshot{State: localstate.Provisioning}}
	done := make(chan error, 1)
	go func() { done <- Submit(ctx, server.URL, "test", store) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != ErrRequestFailed {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation was ignored")
	}
	if store.records != 0 {
		t.Fatal("cancelled request mutated state")
	}
}

func TestSubmitVerifiesTLSAndSanitizesTransportError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS accepted") }))
	defer server.Close()
	store := &memoryStore{snapshot: localstate.Snapshot{State: localstate.Provisioning}}
	for _, url := range []string{server.URL, "http://127.0.0.1:0", "http://user:SECRET@127.0.0.1:0", "http://[invalid"} {
		err := Submit(context.Background(), url, "test", store)
		if err != ErrRequestFailed || strings.Contains(err.Error(), url) || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("unsafe transport error %v", err)
		}
	}
	if store.records != 0 {
		t.Fatal("transport failure mutated state")
	}
}

type failedStore struct {
	memoryStore
	failRead bool
}

func (s *failedStore) Snapshot(ctx context.Context) (localstate.Snapshot, error) {
	if s.failRead {
		return localstate.Snapshot{}, errors.New("SECRET")
	}
	return s.memoryStore.Snapshot(ctx)
}
func (s *failedStore) RecordEnrollmentPending(context.Context, string) (localstate.Snapshot, error) {
	return localstate.Snapshot{}, errors.New("SECRET")
}
func TestSubmitStorageErrorsAreSanitized(t *testing.T) {
	store := &failedStore{memoryStore: memoryStore{snapshot: localstate.Snapshot{NodeID: "node_a", State: localstate.Provisioning}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"node_id":"node_a","enrollment_status":"PENDING"}`))
	}))
	defer server.Close()
	for _, failRead := range []bool{false, true} {
		store.failRead = failRead
		if err := Submit(context.Background(), server.URL, "test", store); err != ErrRequestFailed {
			t.Fatal(err)
		}
	}
	if store.records != 0 {
		t.Fatal("storage failure mutated state")
	}
}

func TestSubmitIgnoresEnvironmentProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyCalls.Add(1); http.Error(w, "proxy", 502) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	// Non-loopback destination would use the proxy if environment discovery were enabled.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	store := &memoryStore{snapshot: localstate.Snapshot{State: localstate.Provisioning}}
	if err := Submit(ctx, "http://192.0.2.1:12345", "test", store); err != ErrRequestFailed {
		t.Fatal(err)
	}
	if proxyCalls.Load() != 0 || store.records != 0 {
		t.Fatal("environment proxy used or state mutated")
	}
}
