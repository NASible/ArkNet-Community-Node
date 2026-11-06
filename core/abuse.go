package main

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Process-local token buckets: bounded memory, no account lockouts or IP logs.
// Restart resets the budgets. Forwarding headers are deliberately untrusted.
const abuseIdleTTL = 10 * time.Minute

type abuseBucket struct {
	tokens float64
	at     time.Time
}

func (b *abuseBucket) refill(now time.Time, perMinute, burst float64) {
	if b.at.IsZero() {
		b.tokens = burst
		b.at = now
		return
	}
	if now.After(b.at) {
		b.tokens = math.Min(burst, b.tokens+now.Sub(b.at).Minutes()*perMinute)
		b.at = now
	}
}

type abuseGate struct {
	mu                                          sync.Mutex
	peers                                       map[string]abuseBucket
	global                                      abuseBucket
	perMinute, burst, globalMinute, globalBurst float64
	capacity                                    int
}

func newAbuseGate(perMinute, burst, globalMinute, globalBurst float64, capacity int) *abuseGate {
	return &abuseGate{peers: make(map[string]abuseBucket), perMinute: perMinute, burst: burst, globalMinute: globalMinute, globalBurst: globalBurst, capacity: capacity}
}

// allow atomically charges both budgets, or neither. Denials don't extend idle
// retention. Full maps reclaim only idle entries, never active depleted peers.
func (g *abuseGate) allow(peer string, now time.Time) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, exists := g.peers[peer]
	if !exists && len(g.peers) >= g.capacity {
		for key, v := range g.peers {
			if now.Sub(v.at) >= abuseIdleTTL {
				delete(g.peers, key)
			}
		}
		if len(g.peers) >= g.capacity {
			return time.Minute
		}
	}
	b.refill(now, g.perMinute, g.burst)
	g.global.refill(now, g.globalMinute, g.globalBurst)
	wait := math.Max((1-b.tokens)/g.perMinute, (1-g.global.tokens)/g.globalMinute)
	if wait > 0 {
		return time.Duration(math.Ceil(wait*60)) * time.Second
	}
	b.tokens--
	g.global.tokens--
	g.peers[peer] = b
	return 0
}

type abuseControls struct {
	login, enrollment, status *abuseGate
	now                       func() time.Time
}

func newAbuseControls(now func() time.Time) *abuseControls {
	return &abuseControls{login: newAbuseGate(10, 5, 60, 10, 4096), enrollment: newAbuseGate(10, 5, 60, 10, 4096), status: newAbuseGate(120, 30, 1200, 120, 4096), now: now}
}
func abusePeer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	return addr.Unmap().WithZone("").String()
}
func (a *abuseControls) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var gate *abuseGate
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && (path == "/api/identity/v1/sessions" || path == nodeLoginPath):
			gate = a.login
		case r.Method == http.MethodPost && path == "/api/federation/v1/enrollments":
			gate = a.enrollment
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/federation/v1/nodes/") && strings.HasSuffix(path, "/enrollment"):
			gate = a.status
		}
		if gate != nil {
			if wait := gate.allow(abusePeer(r), a.now()); wait > 0 {
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Retry-After", strconv.FormatInt(int64(wait/time.Second), 10))
				http.Error(w, "request rate exceeded", http.StatusTooManyRequests)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
