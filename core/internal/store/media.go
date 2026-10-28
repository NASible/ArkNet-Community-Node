// Package store defines ArkNet Core's storage layer.
package store

import (
	"arknet/core/internal/media"
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a storage query finds no matching record.
var ErrNotFound = errors.New("not found")

// ErrReplicaNotEligible means the node/object pair or asserted object metadata
// does not describe an approved node holding the current active catalog object.
var ErrReplicaNotEligible = errors.New("media replica is not eligible")

// MediaReplica records that a node has verified and retained one catalog object.
// It intentionally contains no node-local path or user identity.
type MediaReplica struct {
	NodeID      string
	MediaID     string
	AvailableAt time.Time
}

// Object is an alias for media types to keep imports minimal.
type Object = media.MediaObject

// ObjectState is an alias for media state enum.
type ObjectState = media.ObjectState

const (
	// ObjectActive means the object is catalog-ready for download.
	ObjectActive = media.ObjectActive
	// ObjectRemoved means the object has been logically removed; retained for node cache invalidation.
	ObjectRemoved = media.ObjectRemoved
)

// MediaCatalogRepository is the storage interface for media objects.
type MediaCatalogRepository interface {
	// CreateMediaObject inserts a new media object with a Core-assigned opaque ID.
	CreateMediaObject(ctx context.Context, object Object) (Object, error)
	// GetMediaObjectByPublicID retrieves a single media object by its public ID.
	GetMediaObjectByPublicID(ctx context.Context, id string) (Object, error)
	// ListMediaObjects retrieves up to limit active media objects (or all if includeRemoved is true).
	// Returns ErrInvalidLimit for limit < 1 or limit > 100.
	ListMediaObjects(ctx context.Context, includeRemoved bool, limit int) ([]Object, error)
	// UpdateMediaObjectState atomically transitions the object's state and bumps updated_at.
	UpdateMediaObjectState(ctx context.Context, id string, state ObjectState) (Object, error)
}

// MediaReplicaRepository durably records and reads verified local availability.
type MediaReplicaRepository interface {
	// ReportMediaReplicaAvailable is idempotent for a node/object pair. The size
	// and full v2 hash must exactly match an active catalog object.
	ReportMediaReplicaAvailable(ctx context.Context, nodeID, mediaID string, size int64, infoHashV2 string) (MediaReplica, error)
	// GetMediaReplica proves Core knows that a node reported the object available.
	GetMediaReplica(ctx context.Context, nodeID, mediaID string) (MediaReplica, error)
	// ListEligibleMediaSources returns only opted-in, approved and trusted nodes.
	ListEligibleMediaSources(ctx context.Context, mediaID string, limit int) ([]MediaReplica, error)
}
