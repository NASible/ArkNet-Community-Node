package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Credential-validation fixtures model independent clients, not brute-force
// bursts from one address. Limiter tests below deliberately reuse addresses.
var testClientID atomic.Uint64

func nextTestPeer() string { return fmt.Sprintf("[2001:db8::%x]:1234", testClientID.Add(1)) }

func TestAbuseControlsAreWiredIntoCore(t *testing.T) {
	h := newHandler(nil)
	for i := 0; i < 6; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/api/federation/v1/enrollments", nil))
		if i == 5 && w.Code != http.StatusTooManyRequests {
			t.Fatalf("Core bypassed enrollment limit: %d", w.Code)
		}
	}
}

func TestAbuseLimitsShareLoginBudgetAndRecover(t *testing.T) {
	now := time.Unix(1000, 0)
	calls := 0
	guard := newAbuseControls(func() time.Time { return now })
	h := guard.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	send := func(path, peer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, nil)
		req.RemoteAddr = peer
		req.Header.Set("X-Forwarded-For", fmt.Sprint(calls))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < 5; i++ {
		path := "/node-login"
		if i%2 == 0 {
			path = "/api/identity/v1/sessions"
		}
		if w := send(path, "192.0.2.1:1234"); w.Code != 204 {
			t.Fatal(w.Code)
		}
	}
	w := send("/node-login", "192.0.2.1:9999")
	if w.Code != 429 || w.Header().Get("Retry-After") != "6" || w.Header().Get("Cache-Control") != "no-store" || calls != 5 {
		t.Fatalf("limited: %d %v calls=%d", w.Code, w.Header(), calls)
	}
	if w := send("/api/federation/v1/enrollments", "192.0.2.1:1234"); w.Code != 204 {
		t.Fatal("enrollment shares login budget")
	}
	now = now.Add(6 * time.Second)
	if w := send("/node-login", "192.0.2.1:1234"); w.Code != 204 {
		t.Fatal("did not refill")
	}
}

func TestAbuseGlobalBudgetAndBoundedState(t *testing.T) {
	now := time.Unix(1000, 0)
	guard := newAbuseControls(func() time.Time { return now })
	h := guard.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for i := 0; i < 11; i++ {
		r := httptest.NewRequest("POST", "/node-login", nil)
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1", i+1)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 204
		if i == 10 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("global request %d: %d", i, w.Code)
		}
	}
	// An attacker cannot grow the peer map or evict an active peer to reset it.
	gate := newAbuseGate(10, 5, 60, 10, 2)
	if gate.allow("a", now) != 0 || gate.allow("b", now) != 0 {
		t.Fatal("initial admission")
	}
	if gate.allow("c", now) <= 0 || len(gate.peers) != 2 {
		t.Fatal("capacity not enforced")
	}
	if gate.allow("c", now.Add(11*time.Minute)) != 0 || len(gate.peers) > 2 {
		t.Fatal("idle state not reclaimed")
	}
}

func TestAbuseAdmissionIsAtomic(t *testing.T) {
	gate := newAbuseGate(10, 5, 60, 10, 4096)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if gate.allow("same", time.Unix(1000, 0)) == 0 {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 5 {
		t.Fatalf("accepted %d", accepted.Load())
	}
}

func TestAbuseStatusGlobalBudgetAndRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	g := newAbuseControls(func() time.Time { return now })
	calls := 0
	h := g.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	send := func(i int) int {
		r := httptest.NewRequest("GET", "/api/federation/v1/nodes/"+enrollmentID+"/enrollment", nil)
		r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1", i)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 1; i <= 120; i++ {
		if got := send(i); got != 204 {
			t.Fatalf("poll %d: %d", i, got)
		}
	}
	if got := send(121); got != 429 || calls != 120 {
		t.Fatalf("global denial: %d calls=%d", got, calls)
	}
	now = now.Add(50 * time.Millisecond)
	if got := send(121); got != 204 || calls != 121 {
		t.Fatalf("global refill: %d calls=%d", got, calls)
	}
}

func TestAbusePeerAliasesAndBackwardClockDoNotResetBudget(t *testing.T) {
	now := time.Unix(1000, 0)
	g := newAbuseControls(func() time.Time { return now })
	h := g.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest("POST", "/node-login", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		if i%2 == 1 {
			r.RemoteAddr = "[::ffff:192.0.2.1]:9999"
		}
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 204
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("alias %q: %d", r.RemoteAddr, w.Code)
		}
	}
	gate := newAbuseGate(10, 5, 60, 10, 4096)
	for i := 0; i < 5; i++ {
		if gate.allow("peer", now) != 0 {
			t.Fatal("initial admission")
		}
	}
	if gate.allow("peer", now.Add(-time.Hour)) <= 0 {
		t.Fatal("backward clock created credit")
	}
	if gate.allow("peer", now.Add(6*time.Second)) != 0 {
		t.Fatal("normal clock failed to refill")
	}
}

func TestAbuseStatusPollingAndHealthHaveSeparateBudgets(t *testing.T) {
	now := time.Unix(1000, 0)
	g := newAbuseControls(func() time.Time { return now })
	h := g.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for i := 0; i < 31; i++ {
		r := httptest.NewRequest("GET", "/api/federation/v1/nodes/node_test/enrollment", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 204
		if i == 30 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("poll %d: %d", i, w.Code)
		}
	}
	for _, path := range []string{"/health", "/api/federation/v1/version"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 204 {
			t.Fatal(path, w.Code)
		}
	}
}
