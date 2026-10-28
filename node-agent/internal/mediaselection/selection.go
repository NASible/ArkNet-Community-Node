// Package mediaselection models operator-selected local media metadata only.
package mediaselection

import (
	"arknet/node-agent/internal/catalog"
	"context"
	"errors"
)

type State string

const (
	NotPresent      State = "NOT_PRESENT"
	Queued          State = "QUEUED"
	Downloading     State = "DOWNLOADING"
	Available       State = "AVAILABLE"
	RemovedRetained State = "REMOVED_RETAINED"
	Error           State = "ERROR"
)

var (
	ErrAllocationExceeded = errors.New("media_allocation_exceeded")
	ErrRemoved            = errors.New("media_removed")
	ErrInvalidObject      = errors.New("invalid_media_object")
)

type Object struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	Size       int64         `json:"size"`
	State      catalog.State `json:"state"`
	LocalState State         `json:"local_state"`
}

type Repository interface {
	ListMediaSelections(context.Context) ([]Object, error)
	QueueMediaSelection(context.Context, catalog.Object, int64) (Object, error)
	RetainRemovedMediaSelections(context.Context, []catalog.Object) error
}
