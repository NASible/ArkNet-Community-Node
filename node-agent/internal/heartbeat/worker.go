package heartbeat

import (
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"context"
	"errors"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Storage keeps all state mutations in the persisted CAS state machine.
type Storage interface {
	localstate.Repository
	localstate.StateMachine
	EnrollmentIdentity(context.Context) (enrollment.Identity, error)
	RecordSuccessfulCoreContact(context.Context) (time.Time, error)
	PeerServing(context.Context) (bool, error)
}

// TransitionObserver is called after a heartbeat-driven state transition is persisted.
type TransitionObserver func(from, to localstate.State)

// SuccessHook runs after Core accepts a heartbeat and contact is persisted.
// Hook failure does not rewrite proven heartbeat connectivity.
type SuccessHook func(context.Context, enrollment.Identity) error

// Monitor contains no credentials and is safe for concurrent local status reads.
type Monitor struct {
	mu     sync.RWMutex
	status string
}

func NewMonitor() *Monitor { return &Monitor{status: "UNKNOWN"} }
func (m *Monitor) Connectivity() string {
	if m == nil {
		return "UNKNOWN"
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.status == "" {
		return "UNKNOWN"
	}
	return m.status
}
func (m *Monitor) set(s string) {
	if m != nil {
		m.mu.Lock()
		m.status = s
		m.mu.Unlock()
	}
}

type worker struct {
	store             Storage
	coreURL, version  string
	interval          time.Duration
	failures          int
	monitor           *Monitor
	observeTransition TransitionObserver
	onSuccess         SuccessHook
}

func newWorker(s Storage, coreURL, version string, interval time.Duration, m *Monitor, observers ...TransitionObserver) *worker {
	if interval < 5*time.Second || interval > 5*time.Minute {
		interval = 30 * time.Second
	}
	var observer TransitionObserver
	if len(observers) > 0 {
		observer = observers[0]
	}
	return &worker{store: s, coreURL: coreURL, version: version, interval: interval, monitor: m, observeTransition: observer}
}

func newWorkerWithSuccess(s Storage, coreURL, version string, interval time.Duration, m *Monitor, success SuccessHook, observers ...TransitionObserver) *worker {
	w := newWorker(s, coreURL, version, interval, m, observers...)
	w.onSuccess = success
	return w
}
func allowed(s localstate.State) bool {
	switch s {
	case localstate.Connected, localstate.Synchronizing, localstate.Mesh, localstate.Degraded, localstate.Offline, localstate.Reconnecting:
		return true
	}
	return false
}
func validIdentity(id enrollment.Identity, coreURL, nodeID string) bool {
	if id.Status != "APPROVED" || id.CoreURL != coreURL || len(id.PrivateSeed) != 32 || id.NodeID != nodeID || len(nodeID) != 69 || !strings.HasPrefix(nodeID, "node_") {
		return false
	}
	for _, c := range nodeID[5:] {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	u, err := url.Parse(coreURL)
	return err == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}
func (w *worker) transition(ctx context.Context, from, to localstate.State) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := w.store.Transition(ctx, from, to)
	if err == nil {
		w.monitor.set(string(to))
		if w.observeTransition != nil {
			w.observeTransition(from, to)
		}
	}
	return err == nil
}

// attempt returns false for ineligible/local failures; those never count as failed remote proofs.
func (w *worker) attempt(ctx context.Context) bool {
	if ctx.Err() != nil || w.store == nil || w.coreURL == "" {
		return false
	}
	dbCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	snap, err := w.store.Snapshot(dbCtx)
	if err != nil || !allowed(snap.State) {
		cancel()
		return false
	}
	id, err := w.store.EnrollmentIdentity(dbCtx)
	cancel()
	if err != nil || !validIdentity(id, w.coreURL, snap.NodeID) {
		return false
	}
	state := snap.State
	wasOffline := state == localstate.Offline
	if state == localstate.Degraded || wasOffline {
		if !w.transition(ctx, state, localstate.Reconnecting) {
			return false
		}
		// OFFLINE is durable across Agent restarts. Reconstruct the minimum
		// failure count only after the persisted CAS transition succeeds, so a
		// continuing outage cannot incorrectly regress it to DEGRADED.
		if wasOffline && w.failures < 2 {
			w.failures = 2
		}
		state = localstate.Reconnecting
	}
	dbCtx, cancel = context.WithTimeout(ctx, 2*time.Second)
	healthy := w.store.Ping(dbCtx) == nil
	peerServing, settingsErr := w.store.PeerServing(dbCtx)
	cancel()
	if ctx.Err() != nil || settingsErr != nil {
		return false
	}
	err = send(ctx, id, w.version, state, healthy, peerServing)
	if ctx.Err() != nil {
		return false
	}
	if err == nil {
		dbCtx, cancel = context.WithTimeout(ctx, 2*time.Second)
		_, recordErr := w.store.RecordSuccessfulCoreContact(dbCtx)
		cancel()
		if recordErr != nil {
			return true
		}
		if w.onSuccess != nil {
			refreshCtx, refreshCancel := context.WithTimeout(ctx, 10*time.Second)
			_ = w.onSuccess(refreshCtx, id)
			refreshCancel()
		}
		if state == localstate.Reconnecting && !w.transition(ctx, state, localstate.Connected) {
			return true
		}
		w.failures = 0
		w.monitor.set("CONNECTED")
		return true
	}
	if errors.Is(err, ErrNotApproved) {
		if decision, statusErr := enrollment.FederationStatus(ctx, id); statusErr == nil && decision == "REVOKED/BLOCKED" {
			// Authoritative, signed confirmation: REVOKED is terminal and stops heartbeats.
			if w.transition(ctx, state, localstate.Revoked) {
				w.failures = 0
				w.monitor.set(string(localstate.Revoked))
				return true
			}
		}
	}
	w.failures++
	next := localstate.Degraded
	if w.failures >= 3 {
		next = localstate.Offline
	}
	w.transition(ctx, state, next)
	return true
}

// backoff never decreases the base interval, and caps both exponential growth and jitter.
func backoff(interval time.Duration, failures int, jitter float64) time.Duration {
	base := interval
	for i := 1; i < failures && base < 5*time.Minute; i++ {
		base *= 2
		if base > 5*time.Minute {
			base = 5 * time.Minute
		}
	}
	if jitter < 0 {
		jitter = 0
	}
	if jitter > 1 {
		jitter = 1
	}
	delay := base + time.Duration(float64(base)*.1*jitter)
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	return delay
}

// Run sends immediately, then at bounded intervals. Core outages never escape into
// the local HTTP server. Every operation and timer is canceled by the joined worker context.
func Run(ctx context.Context, coreURL, version string, store Storage, interval time.Duration, monitor *Monitor, observers ...TransitionObserver) {
	run(ctx, coreURL, version, store, interval, monitor, nil, observers...)
}

// RunWithSuccess adds post-heartbeat reconciliation without changing the
// heartbeat protocol or treating refresh failure as transport failure.
func RunWithSuccess(ctx context.Context, coreURL, version string, store Storage, interval time.Duration, monitor *Monitor, success SuccessHook, observers ...TransitionObserver) {
	run(ctx, coreURL, version, store, interval, monitor, success, observers...)
}

func run(ctx context.Context, coreURL, version string, store Storage, interval time.Duration, monitor *Monitor, success SuccessHook, observers ...TransitionObserver) {
	if coreURL == "" || store == nil {
		return
	}
	w := newWorkerWithSuccess(store, coreURL, version, interval, monitor, success, observers...)
	for ctx.Err() == nil {
		w.attempt(ctx)
		delay := w.interval
		if w.failures > 0 {
			delay = backoff(w.interval, w.failures, rand.Float64())
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
