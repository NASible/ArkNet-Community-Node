package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"arknet/core/internal/media"
	"arknet/core/internal/nodes"
	"arknet/core/internal/routing"
)

var _ routing.Repository = (*Store)(nil)
var _ routing.MissRepository = (*Store)(nil)

// RecordMediaMiss upserts one aggregate row. The predicate re-checks, at write
// time, that the Node is trusted, the object ACTIVE, and that the Node still
// has no verified replica, so a stale miss cannot be recorded.
func (s *Store) RecordMediaMiss(ctx context.Context, nodeID, mediaID string) error {
	if !nodes.ValidPublicID(nodeID) || !media.ValidMediaID(mediaID) {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO media_miss_events (node_id, media_object_id)
SELECT n.id, m.id FROM nodes n, media_objects m
WHERE n.public_id = $1 AND n.enrollment_status = 'APPROVED' AND n.trust_status = 'TRUSTED'
  AND m.public_id = $2 AND m.state = 'ACTIVE'
  AND NOT EXISTS (SELECT 1 FROM media_replicas r WHERE r.node_id = n.id AND r.media_object_id = m.id)
ON CONFLICT (node_id, media_object_id) DO UPDATE
SET miss_count = CASE WHEN media_miss_events.miss_count < 9223372036854775807 THEN media_miss_events.miss_count + 1 ELSE media_miss_events.miss_count END,
    last_missed_at = now()`, nodeID, mediaID)
	if err != nil {
		return fmt.Errorf("record media miss: %w", err)
	}
	return nil
}

// ListMediaMisses returns only this Node's misses for objects that are still
// ACTIVE and that the Node still lacks, most frequent first.
func (s *Store) ListMediaMisses(ctx context.Context, nodeID string, limit int) ([]routing.MediaMiss, error) {
	if !nodes.ValidPublicID(nodeID) {
		return nil, nodes.ErrInvalidPublicID
	}
	if limit < 1 || limit > routing.MaxListedMisses {
		return nil, media.ErrInvalidLimit
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT m.public_id, e.miss_count
FROM media_miss_events e
JOIN nodes n ON n.id = e.node_id
JOIN media_objects m ON m.id = e.media_object_id
WHERE n.public_id = $1 AND n.enrollment_status = 'APPROVED' AND n.trust_status = 'TRUSTED'
  AND m.state = 'ACTIVE'
  AND NOT EXISTS (SELECT 1 FROM media_replicas r WHERE r.node_id = n.id AND r.media_object_id = m.id)
ORDER BY e.miss_count DESC, m.public_id
LIMIT $2`, nodeID, limit)
	if err != nil {
		return nil, fmt.Errorf("list media misses: %w", err)
	}
	defer rows.Close()
	misses := []routing.MediaMiss{}
	for rows.Next() {
		var miss routing.MediaMiss
		if err := rows.Scan(&miss.MediaID, &miss.MissCount); err != nil {
			return nil, fmt.Errorf("list media misses: %w", err)
		}
		misses = append(misses, miss)
	}
	return misses, rows.Err()
}

// HomeNodeStatus evaluates health and availability in one snapshot. Health
// uses only Core-side facts: trust state, the last accepted heartbeat's state
// and health report, and Core's own last_seen_at clock. Availability requires a
// verified replica report for an object that is still ACTIVE, so a removed
// object never routes to a Node even though its replica row is retained.
func (s *Store) HomeNodeStatus(ctx context.Context, nodeID, mediaID string, maxHeartbeatAge time.Duration) (routing.HomeNodeStatus, error) {
	if !nodes.ValidPublicID(nodeID) || !media.ValidMediaID(mediaID) || maxHeartbeatAge <= 0 {
		return routing.HomeNodeStatus{}, nil
	}
	var status routing.HomeNodeStatus
	err := s.db.QueryRowContext(ctx, `
SELECT
    n.enrollment_status = 'APPROVED' AND n.trust_status = 'TRUSTED'
    AND n.current_state IN ('CONNECTED', 'SYNCHRONIZING', 'MESH')
    AND COALESCE((n.reported_health->>'healthy')::boolean, false)
    AND COALESCE((n.reported_health->>'database_healthy')::boolean, false)
    AND n.last_seen_at IS NOT NULL
    AND n.last_seen_at >= clock_timestamp() - make_interval(secs => $3),
    EXISTS (
        SELECT 1 FROM media_replicas r JOIN media_objects m ON m.id = r.media_object_id
        WHERE r.node_id = n.id AND m.public_id = $2 AND m.state = 'ACTIVE' AND m.info_hash_v2 IS NOT NULL
    )
FROM nodes n
WHERE n.public_id = $1`, nodeID, mediaID, maxHeartbeatAge.Seconds()).Scan(&status.Healthy, &status.VerifiedAvailable)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.HomeNodeStatus{}, nil
	}
	if err != nil {
		return routing.HomeNodeStatus{}, fmt.Errorf("home node routing status: %w", err)
	}
	return status, nil
}
