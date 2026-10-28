// Package localstate defines the Node Agent's persisted local identity and state.
package localstate

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type State string

const (
	Provisioning      State = "PROVISIONING"
	EnrollmentPending State = "ENROLLMENT_PENDING"
	Connected         State = "CONNECTED"
	Synchronizing     State = "SYNCHRONIZING"
	Mesh              State = "MESH"
	Degraded          State = "DEGRADED"
	Offline           State = "OFFLINE"
	Reconnecting      State = "RECONNECTING"
	Quarantined       State = "QUARANTINED"
	Revoked           State = "REVOKED"
	Error             State = "ERROR"
)

var (
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrStateConflict     = errors.New("state conflict")
)

var transitions = map[State][]State{
	Provisioning:      {EnrollmentPending, Error},
	EnrollmentPending: {Connected, Offline, Quarantined, Revoked, Error},
	Connected:         {Synchronizing, Degraded, Offline, Quarantined, Revoked, Error},
	Synchronizing:     {Mesh, Degraded, Offline, Quarantined, Revoked, Error},
	Mesh:              {Synchronizing, Degraded, Offline, Quarantined, Revoked, Error},
	Degraded:          {Reconnecting, Offline, Quarantined, Revoked, Error},
	Offline:           {Reconnecting, Quarantined, Revoked, Error},
	Reconnecting:      {EnrollmentPending, Connected, Offline, Degraded, Quarantined, Revoked, Error},
	Quarantined:       {Reconnecting, Revoked},
	Revoked:           {},
	Error:             {Provisioning, Reconnecting, Quarantined, Revoked},
}

func (s State) Valid() bool { _, ok := transitions[s]; return ok }

func Validate(from, to State) error {
	if from.Valid() && to.Valid() {
		for _, allowed := range transitions[from] {
			if to == allowed {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: %q -> %q", ErrInvalidTransition, from, to)
}

type Snapshot struct {
	NodeID                      string
	State                       State
	StateChangedAt              time.Time
	EnrollmentState             string
	LastSuccessfulCoreContactAt time.Time
}

type Repository interface {
	Ping(context.Context) error
	Snapshot(context.Context) (Snapshot, error)
}

// StateMachine is separate from the read-only handler repository. Transition
// compares expected against persisted state before applying the transition graph.
type StateMachine interface {
	Transition(ctx context.Context, expected State, next State) (Snapshot, error)
}
