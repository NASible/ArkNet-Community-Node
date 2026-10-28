// Package nodes defines ArkNet-managed node records and their storage boundary.
package nodes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

type EnrollmentStatus string
type TrustStatus string

const (
	EnrollmentPending  EnrollmentStatus = "PENDING"
	EnrollmentApproved EnrollmentStatus = "APPROVED"
	EnrollmentRejected EnrollmentStatus = "REJECTED"
	// EnrollmentRevoked withdraws a previously approved Node. Every federation
	// endpoint (tracker grants, catalog, heartbeats, invitations, replicas,
	// offline authorizations) requires APPROVED/TRUSTED, so a revoked Node is
	// denied new swarm authorization immediately.
	EnrollmentRevoked EnrollmentStatus = "REVOKED"
	TrustUntrusted    TrustStatus      = "UNTRUSTED"
	TrustTrusted      TrustStatus      = "TRUSTED"
	TrustBlocked      TrustStatus      = "BLOCKED"
)

var (
	ErrHeartbeatReplay        = errors.New("heartbeat proof is not newer")
	ErrNodeAuthentication     = errors.New("node authentication failed")
	ErrNodeUnapproved         = errors.New("node is not approved")
	ErrInvalidState           = errors.New("invalid node state")
	ErrNotFound               = errors.New("node not found")
	ErrEnrollmentConflict     = errors.New("node enrollment conflicts with existing state")
	ErrInvalidPublicID        = errors.New("invalid public node ID")
	ErrInvalidPage            = errors.New("invalid node page")
	ErrInvalidSoftwareVersion = errors.New("software version must not be empty")
)

type BasicHealth struct {
	Healthy         bool `json:"healthy"`
	DatabaseHealthy bool `json:"database_healthy"`
}

type Node struct {
	CurrentState     *string
	ReportedHealth   *BasicHealth
	ID               int64
	PublicID         string
	EnrollmentStatus EnrollmentStatus
	TrustStatus      TrustStatus
	SoftwareVersion  string
	CreatedAt        time.Time
	LastSeenAt       *time.Time
}

// Repository is the original internally generated node creation/read boundary.
// Create generates a public ID; callers cannot supply IDs or promote state.
// Enrollment with an existing local opaque ID uses EnrollmentRepository instead.
type Repository interface {
	Create(ctx context.Context, softwareVersion string) (Node, error)
	GetByPublicID(ctx context.Context, publicID string) (Node, error)
}

// EnrollmentRepository accepts only opaque public IDs and software versions.
// Repeated enrollment must not change an existing node record.
type EnrollmentRepository interface {
	Enroll(ctx context.Context, publicID, softwareVersion string) (Node, error)
}

// HandshakeRepository pins a node-specific key only while pending/untrusted.
// Registration retries never change metadata or decided state.
type HandshakeRepository interface {
	RegisterEnrollment(ctx context.Context, nodeID, version string, publicKey []byte) (Node, error)
	EnrollmentIdentity(ctx context.Context, nodeID string) (Node, []byte, error)
}

// AdministrationRepository permits only decisions on pending, untrusted nodes.
// ListPending accepts an empty or exact valid public ID cursor and a limit of
// 1..100, returning up to limit+1 rows ordered by public ID. Callers truncate the
// lookahead row and emit the last retained public ID as the next cursor.
// Approve and Reject preserve all metadata and conflict on any decided state.
type AdministrationRepository interface {
	ListPending(ctx context.Context, after string, limit int) ([]Node, error)
	Approve(ctx context.Context, publicID string) (Node, error)
	Reject(ctx context.Context, publicID string) (Node, error)
	Revoke(ctx context.Context, publicID string) (Node, error)
}

// MetadataRepository is the read-only boundary for the protected admin detail.
type MetadataRepository interface {
	GetByPublicID(ctx context.Context, id string) (Node, error)
}

// HeartbeatRepository mutates liveness only for an authenticated, approved,
// bound identity and a strictly newer proof timestamp. Implementations serialize
// the authorization conditions and metadata update atomically.
type HeartbeatRepository interface {
	HandshakeRepository
	AcceptHeartbeat(ctx context.Context, id, version, state string, healthy, databaseHealthy, peerServing bool, key []byte, proofAt int64) error
}

// ValidState mirrors the eleven public Node Agent localstate states. It is
// reported metadata, not authority to change Core enrollment or trust.
func ValidState(state string) bool {
	switch state {
	case "PROVISIONING", "ENROLLMENT_PENDING", "CONNECTED", "SYNCHRONIZING", "MESH", "DEGRADED", "OFFLINE", "RECONNECTING", "QUARANTINED", "REVOKED", "ERROR":
		return true
	}
	return false
}

// NewPublicID uses 256 random bits, with no operator-provided identifying input.
func NewPublicID() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "node_" + hex.EncodeToString(random[:]), nil
}

// ValidSoftwareVersion bounds an opaque release marker without inspecting identity.
func ValidSoftwareVersion(version string) bool {
	if len(version) == 0 || len(version) > 64 || strings.TrimSpace(version) == "" {
		return false
	}
	for _, c := range []byte(version) {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

func ValidPublicID(id string) bool {
	if len(id) != 69 || !strings.HasPrefix(id, "node_") {
		return false
	}
	for _, c := range id[5:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
