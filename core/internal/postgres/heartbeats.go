package postgres

import (
	"arknet/core/internal/nodes"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
)

var _ nodes.HeartbeatRepository = (*Store)(nil)

// A conditional UPDATE takes PostgreSQL's row lock and rechecks all predicates
// after a competing writer commits. Never split the replay check from mutation.
func (s *Store) AcceptHeartbeat(ctx context.Context, id, version, state string, healthy, databaseHealthy, peerServing bool, key []byte, proofAt int64) error {
	if !nodes.ValidPublicID(id) {
		return nodes.ErrInvalidPublicID
	}
	if !nodes.ValidSoftwareVersion(version) {
		return nodes.ErrInvalidSoftwareVersion
	}
	if !nodes.ValidState(state) {
		return nodes.ErrInvalidState
	}
	if len(key) != ed25519.PublicKeySize {
		return nodes.ErrNodeAuthentication
	}
	if s == nil || s.db == nil {
		return errors.New("heartbeat database unavailable")
	}
	health, _ := json.Marshal(nodes.BasicHealth{Healthy: healthy, DatabaseHealthy: databaseHealthy})
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET software_version=$2,current_state=$3,reported_health=$4::jsonb,peer_serving=$7,heartbeat_proof_at=$6,last_seen_at=clock_timestamp()
 WHERE public_id=$1 AND enrollment_status='APPROVED' AND trust_status='TRUSTED'
 AND enrollment_public_key=$5 AND (heartbeat_proof_at IS NULL OR heartbeat_proof_at<$6)`, id, version, state, string(health), key, proofAt, peerServing)
	if err != nil {
		return errors.New("heartbeat update failed")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return errors.New("heartbeat update failed")
	}
	if changed == 1 {
		return nil
	}
	// Read only to classify the unsuccessful update; no metadata can be changed.
	node, stored, err := s.EnrollmentIdentity(ctx, id)
	if err != nil {
		return err
	}
	if len(stored) != ed25519.PublicKeySize || !bytes.Equal(stored, key) {
		return nodes.ErrNodeAuthentication
	}
	if node.EnrollmentStatus != nodes.EnrollmentApproved || node.TrustStatus != nodes.TrustTrusted {
		return nodes.ErrNodeUnapproved
	}
	return nodes.ErrHeartbeatReplay
}
