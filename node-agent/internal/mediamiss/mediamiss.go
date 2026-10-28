// Package mediamiss models Node-local handling of Core media-miss events:
// active ArkNet objects this Node's home users had to fetch from Core.
package mediamiss

import (
	"context"
	"errors"
)

type Policy string

const (
	// Manual: misses are not surfaced; the operator curates media directly.
	Manual Policy = "MANUAL"
	// Ask: misses become operator notifications (the MVP default).
	Ask Policy = "ASK"
	// Automatic: misses are queued and started within quota, availability,
	// and the one-transfer-at-a-time bandwidth policy.
	Automatic Policy = "AUTOMATIC"
	Default          = Ask
)

var ErrNotFound = errors.New("media miss notification not found")

func ValidPolicy(policy Policy) bool {
	return policy == Manual || policy == Ask || policy == Automatic
}

// Notification is what the operator sees. Title and size come from the
// authorized Core catalog, never from Core's miss report or the browser.
type Notification struct {
	MediaID   string `json:"media_id"`
	Title     string `json:"title"`
	Size      int64  `json:"size"`
	MissCount int64  `json:"miss_count"`
}

type Repository interface {
	MediaMissPolicy(context.Context) (Policy, error)
	SetMediaMissPolicy(context.Context, Policy) error
	// StoreMediaMisses upserts pending notifications; ignored ones stay ignored.
	StoreMediaMisses(context.Context, []Notification) error
	// PendingMediaMisses excludes ignored objects and any object that already
	// has a local selection (queued, downloading, available, or retained).
	PendingMediaMisses(context.Context) ([]Notification, error)
	IgnoreMediaMiss(context.Context, string) error
}
