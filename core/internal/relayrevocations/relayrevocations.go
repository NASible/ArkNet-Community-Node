// Package relayrevocations is the storage contract for pushing Node
// revocations to operator relays (Prompt 48B).
//
// When a Node becomes BLOCKED (revoked or rejected), the store records a
// revocation event in the same transaction. Each configured relay must then
// acknowledge it. A delivery is pending until its relay acknowledges; relays
// are identified by their configured IDs, so a relay added later only sees
// events it has not acknowledged.
package relayrevocations

import (
	"context"
	"time"
)

// Task is one undelivered (Node, relay) revocation.
type Task struct {
	NodeID        string
	RelayID       string
	DecidedAt     time.Time
	Attempts      int
	LastError     string // short code, never a subject or URL
	LastAttemptAt time.Time
}

type Repository interface {
	// PendingRelayRevocations lists unacknowledged deliveries for the given
	// relay IDs, oldest decision first.
	PendingRelayRevocations(ctx context.Context, relayIDs []string, limit int) ([]Task, error)
	// RecordRelayRevocationAttempt records one attempt; an empty failure
	// means the relay acknowledged.
	RecordRelayRevocationAttempt(ctx context.Context, nodeID, relayID, failure string) error
	// MediaInfoHashes lists the v2 info hash of every catalog object in any
	// state, since grants for removed objects can still be live.
	MediaInfoHashes(ctx context.Context) ([]string, error)
}
