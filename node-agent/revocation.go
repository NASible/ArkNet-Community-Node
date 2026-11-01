package main

import (
	"sync"

	"arknet/node-agent/internal/heartbeat"
	"arknet/node-agent/internal/localstate"
)

// wakeup lets the heartbeat worker run the transfer and seed reconcilers
// immediately instead of at their next interval. Signals coalesce: a
// reconciler that is already due runs once.
type wakeup struct {
	mu       sync.Mutex
	channels []chan struct{}
}

func newWakeup() *wakeup { return &wakeup{} }

// channel registers one reconciler.
func (w *wakeup) channel() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch := make(chan struct{}, 1)
	w.channels = append(w.channels, ch)
	return ch
}

func (w *wakeup) signal() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.channels {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// revocationObserver wakes the reconcilers when the Node enters terminal
// REVOKED, so active transfers and seeds stop within one heartbeat of
// Core's authoritative decision rather than one reconcile interval later.
func revocationObserver(w *wakeup) heartbeat.TransitionObserver {
	return func(_, to localstate.State) {
		if to == localstate.Revoked {
			w.signal()
		}
	}
}

func observeAll(observers ...heartbeat.TransitionObserver) heartbeat.TransitionObserver {
	return func(from, to localstate.State) {
		for _, observe := range observers {
			if observe != nil {
				observe(from, to)
			}
		}
	}
}
