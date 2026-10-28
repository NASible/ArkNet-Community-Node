// Package engine is an in-memory MOCK simulator, not a transfer or verification implementation.
package engine

import (
	"errors"
	"math/bits"
	"sort"
	"sync"
	"time"
)

type State string

const (
	Downloading        State = "DOWNLOADING"
	Paused             State = "PAUSED"
	Completed          State = "COMPLETED"
	Cancelled          State = "CANCELLED"
	VerificationFailed State = "VERIFICATION_FAILED"
)

var (
	ErrNotFound   = errors.New("not_found")
	ErrConflict   = errors.New("conflict")
	ErrTransition = errors.New("invalid_transition")
	ErrInvalid    = errors.New("invalid_request")
)

type Status struct {
	ObjectID       string `json:"object_id"`
	State          State  `json:"state"`
	TotalBytes     int64  `json:"total_bytes"`
	CompletedBytes int64  `json:"completed_bytes"`
	Mock           bool   `json:"mock"`
	ErrorCode      string `json:"error_code"`
}
type entry struct {
	status  Status
	hash    string
	elapsed time.Duration
	last    time.Time
}

// Engine serializes all operations, returning coherent value snapshots.
// A bounded map and lazy wall-clock advancement require no background goroutines.
type Engine struct {
	mu       sync.Mutex
	entries  map[string]*entry
	duration time.Duration
	now      func() time.Time
}

func New(duration time.Duration, now func() time.Time) *Engine {
	if duration <= 0 {
		panic("positive mock duration required")
	}
	if now == nil {
		now = time.Now
	}
	return &Engine{entries: make(map[string]*entry), duration: duration, now: now}
}
func (e *Engine) refresh(v *entry) {
	if v.status.State != Downloading {
		return
	}
	now := e.now()
	delta := now.Sub(v.last)
	if delta <= 0 {
		return
	}
	v.last = now
	remaining := e.duration - v.elapsed
	if delta >= remaining {
		v.elapsed = e.duration
		v.status.CompletedBytes = v.status.TotalBytes
		v.status.State = Completed
		return
	}
	v.elapsed += delta
	// Full-width integer multiplication avoids float rounding and int64 overflow.
	hi, lo := bits.Mul64(uint64(v.status.TotalBytes), uint64(v.elapsed))
	progress, _ := bits.Div64(hi, lo, uint64(e.duration))
	if int64(progress) > v.status.CompletedBytes {
		v.status.CompletedBytes = int64(progress)
	}
}
func (e *Engine) Add(id string, size int64, hash string) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !ValidObjectID(id) || size < 0 || (hash != "" && !LowerHex(hash)) {
		return Status{}, ErrInvalid
	}
	if v, ok := e.entries[id]; ok {
		if v.status.TotalBytes != size || v.hash != hash {
			return Status{}, ErrConflict
		}
		e.refresh(v)
		return v.status, nil
	}
	if len(e.entries) >= 4096 {
		return Status{}, ErrConflict
	}
	v := &entry{status: Status{ObjectID: id, State: Downloading, TotalBytes: size, Mock: true}, hash: hash, last: e.now()}
	e.entries[id] = v
	return v.status, nil
}
func (e *Engine) Get(id string) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.entries[id]
	if !ok {
		return Status{}, ErrNotFound
	}
	e.refresh(v)
	return v.status, nil
}
func (e *Engine) List() []Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Status, 0, len(e.entries))
	for _, v := range e.entries {
		e.refresh(v)
		out = append(out, v.status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObjectID < out[j].ObjectID })
	return out
}
func (e *Engine) Action(id, action string) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.entries[id]
	if !ok {
		return Status{}, ErrNotFound
	}
	e.refresh(v)
	state := v.status.State
	switch action {
	case "pause":
		if state != Downloading && state != Paused {
			return Status{}, ErrTransition
		}
		v.status.State = Paused
	case "resume":
		if state != Paused && state != Downloading {
			return Status{}, ErrTransition
		}
		if state == Paused {
			v.last = e.now()
		}
		v.status.State = Downloading
	case "cancel":
		if state != Downloading && state != Paused && state != Cancelled {
			return Status{}, ErrTransition
		}
		v.status.State = Cancelled
	default:
		return Status{}, ErrInvalid
	}
	return v.status, nil
}
