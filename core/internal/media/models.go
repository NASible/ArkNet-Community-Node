// Package media defines ArkNet media objects and their storage boundary.
//
// Media objects use an opaque, Core-assigned identifier so node filesystem
// paths never become global identity. The state machine allows only ACTIVE
// (the default) and REMOVED (terminal); implementations must reject unknown
// transitions.
package media

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// ObjectType enumerates the top-level kind of a media object.
type ObjectType string

const (
	// ObjectTypeVideo covers files intended primarily for video playback
	// (e.g. movies, series episodes, documentaries).
	ObjectTypeVideo ObjectType = "VIDEO"
	// ObjectTypeAudio covers music, podcasts, and other listen-only content.
	ObjectTypeAudio ObjectType = "AUDIO"
	// ObjectTypeImage covers covers, posters, and still images.
	ObjectTypeImage ObjectType = "IMAGE"
	// ObjectTypeOther covers anything that doesn't fit the above categories.
	ObjectTypeOther ObjectType = "OTHER"
)

func validObjectType(t ObjectType) bool {
	switch t {
	case ObjectTypeVideo, ObjectTypeAudio, ObjectTypeImage, ObjectTypeOther:
		return true
	}
	return false
}

// ObjectState is the lifecycle stage of a media object.
type ObjectState string

const (
	// ObjectActive means the object is available for catalog display and
	// download. This is the default insertion state.
	ObjectActive ObjectState = "ACTIVE"
	// ObjectRemoved means the object has been logically removed. Its row
	// remains to preserve the identifier for nodes that have already cached
	// or partially transferred it.
	ObjectRemoved ObjectState = "REMOVED"
)

func validState(s ObjectState) bool {
	switch s {
	case ObjectActive, ObjectRemoved:
		return true
	}
	return false
}

// APIObject is the JSON response representation of a media object.
type APIObject struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	Title       string `json:"title"`
	Size        int64  `json:"size"`
	State       string `json:"state"`
	InfoHashV2  string `json:"info_hash_v2"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// MediaObject is an ArkNet-managed media record stored on Core.
type MediaObject struct {
	// ID is the opaque, globally unique identifier assigned by Core.
	ID string
	// ObjectType classifies the content category.
	ObjectType ObjectType
	// Title is the human-readable name.
	Title string
	// Size is the total byte count. Zero is allowed when the file size is
	// unknown at creation time.
	Size int64
	// State tracks where the object is in its lifecycle.
	State ObjectState
	// InfoHashV2 is the BitTorrent v2 (SHA2-256) infohash. It is 64
	// hexadecimal characters when present, empty when the torrent is not
	// yet assigned.
	InfoHashV2 string
	// CreatedAt is when Core created the record.
	CreatedAt time.Time
	// UpdatedAt is when Core last modified the record.
	UpdatedAt time.Time
}

// AsAPIView returns the APIObject representation of the media record.
func (m MediaObject) AsAPIView() APIObject {
	return APIObject{
		ID:          m.ID,
		ContentType: string(m.ObjectType),
		Title:       m.Title,
		Size:        m.Size,
		State:       string(m.State),
		InfoHashV2:  m.InfoHashV2,
		CreatedAt:   m.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   m.UpdatedAt.Format(time.RFC3339),
	}
}

// ErrInvalidObjectType is returned when a caller supplies an unrecognised
// content type.
var ErrInvalidObjectType = fmt.Errorf("invalid object type")

// ErrInvalidState is returned for an illegal state transition or unknown
// state value.
var ErrInvalidState = fmt.Errorf("invalid state")

// ErrInvalidInfoHashV2 is returned when the provided string is not a valid
// 64-character lowercase hexadecimal SHA2-256 infohash.
var ErrInvalidInfoHashV2 = fmt.Errorf("invalid info hash v2")

// ErrInvalidSize is returned when a media object has a negative byte size.
var ErrInvalidSize = fmt.Errorf("invalid media size")

// InfoHashV2Length is the expected length of a SHA2-256 infohash in hex.
const InfoHashV2Length = 64

// Validate checks whether o contains only recognised, parseable values. It
// does not assert uniqueness or existence; callers handle those via the
// storage layer.
func Validate(o MediaObject) error {
	if o.Size < 0 {
		return ErrInvalidSize
	}
	if !validObjectType(o.ObjectType) {
		return ErrInvalidObjectType
	}
	if !validState(o.State) {
		return ErrInvalidState
	}
	if o.InfoHashV2 != "" {
		if len(o.InfoHashV2) != InfoHashV2Length || strings.Contains(o.InfoHashV2, " ") {
			return ErrInvalidInfoHashV2
		}
		for _, c := range o.InfoHashV2 {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return ErrInvalidInfoHashV2
			}
		}
	}
	return nil
}

// generateMediaID creates an opaque, collision-resistant identifier using
// 256 random bits. This deliberately excludes any filesystem path or
// operator-supplied component.
func generateMediaID() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate media ID: %w", err)
	}
	return "media_" + hex.EncodeToString(random[:]), nil
}

// GenerateMediaID is the exported factory used by the storage layer.
func GenerateMediaID() (string, error) {
	return generateMediaID()
}

// ErrInvalidLimit is returned when a List call specifies limit < 1 or limit > 100.
var ErrInvalidLimit = fmt.Errorf("invalid limit: must be between 1 and 100")

// ErrInvalidMediaID is returned when the provided ID does not match the
// expected "media_<64 hex chars>" format enforced by the database.
var ErrInvalidMediaID = fmt.Errorf("invalid media ID")

// ValidMediaID checks whether id matches the database CHECK constraint format:
// "media_" followed by exactly 64 lowercase hexadecimal characters.
func ValidMediaID(id string) bool {
	if len(id) != 70 || id[:6] != "media_" {
		return false
	}
	for _, c := range id[6:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ClockNow is a helper that the storage layer calls to obtain the current time.
// It is exported so tests can inject a deterministic clock when needed.
func ClockNow() time.Time {
	return time.Now().UTC()
}
