// Package routing decides where an authenticated global user should obtain an
// active ArkNet media object: their healthy home Node when it has verified the
// object locally, otherwise Core.
package routing

import (
	"context"
	"time"
)

type Target string

const (
	TargetHomeNode Target = "HOME_NODE"
	TargetCore     Target = "CORE"
)

// MaxHeartbeatAge bounds how recently Core must have accepted a heartbeat
// from the home Node: three default 30-second Agent intervals. A Node with a
// longer interval, or whose last heartbeat is older, routes to Core.
const MaxHeartbeatAge = 90 * time.Second

// HomeNodeStatus holds the two facts the decision needs. Both are Core
// observations: accepted heartbeats and verified replica reports.
type HomeNodeStatus struct {
	Healthy           bool
	VerifiedAvailable bool
}

// Decision is the minimal routing result. NodeID is set only for
// TargetHomeNode and is always the caller's own home Node.
type Decision struct {
	Target Target
	NodeID string
}

// Repository reads a single Node's routing status for one media object.
// An unknown Node reports neither healthy nor available.
type Repository interface {
	HomeNodeStatus(ctx context.Context, nodeID, mediaID string, maxHeartbeatAge time.Duration) (HomeNodeStatus, error)
}

// MaxListedMisses bounds one Node's media-miss listing.
const MaxListedMisses = 100

// MediaMiss is the only miss data a Node receives: which active object its
// users fell back to Core for, and how often. No user, member, or time data.
type MediaMiss struct {
	MediaID   string
	MissCount int64
}

// MissRecorder records that a home Node lacked an active object. It is called
// off the request path, so its latency never affects a routing response.
type MissRecorder interface {
	RecordMediaMiss(ctx context.Context, nodeID, mediaID string) error
}

// MissRepository adds the Node-scoped read used by the federation endpoint.
type MissRepository interface {
	MissRecorder
	ListMediaMisses(ctx context.Context, nodeID string, limit int) ([]MediaMiss, error)
}

// IsMiss reports whether a Core fallback happened because the user's home
// Node lacks the object. An offline home Node that has the object is not a
// miss; no home Node at all is not a miss.
func IsMiss(homeNodeID string, status HomeNodeStatus) bool {
	return homeNodeID != "" && !status.VerifiedAvailable
}

// Decide applies the routing order: no home node or an unhealthy home node
// selects Core; a healthy home node without verified local availability
// selects Core; otherwise prefer the home node.
func Decide(homeNodeID string, status HomeNodeStatus) Decision {
	if homeNodeID == "" || !status.Healthy || !status.VerifiedAvailable {
		return Decision{Target: TargetCore}
	}
	return Decision{Target: TargetHomeNode, NodeID: homeNodeID}
}
