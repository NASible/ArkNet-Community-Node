package postgres

import (
	"arknet/core/internal/media"
	"arknet/core/internal/nodes"
	"arknet/core/internal/store"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var _ store.MediaReplicaRepository = (*Store)(nil)

// ReportMediaReplicaAvailable inserts at most one row for the node/object pair.
// The INSERT predicate repeats all authorization and catalog-integrity checks so
// a concurrent catalog or node-state change cannot create a stale replica row.
func (s *Store) ReportMediaReplicaAvailable(ctx context.Context, nodeID, mediaID string, size int64, infoHashV2 string) (store.MediaReplica, error) {
	if !nodes.ValidPublicID(nodeID) {
		return store.MediaReplica{}, nodes.ErrInvalidPublicID
	}
	if !media.ValidMediaID(mediaID) {
		return store.MediaReplica{}, media.ErrInvalidMediaID
	}
	if size < 0 {
		return store.MediaReplica{}, media.ErrInvalidSize
	}
	if !validReplicaInfoHashV2(infoHashV2) {
		return store.MediaReplica{}, media.ErrInvalidInfoHashV2
	}
	var replica store.MediaReplica
	err := s.db.QueryRowContext(ctx, `
WITH eligible AS (
    SELECT n.id AS node_id, m.id AS media_object_id
    FROM nodes n, media_objects m
    WHERE n.public_id = $1
      AND n.enrollment_status = 'APPROVED'
      AND n.trust_status = 'TRUSTED'
      AND m.public_id = $2
      AND m.state = 'ACTIVE'
      AND m.size = $3
      AND m.info_hash_v2 = $4
), upserted AS (
    INSERT INTO media_replicas (node_id, media_object_id)
    SELECT node_id, media_object_id FROM eligible
    WHERE true
    ON CONFLICT (node_id, media_object_id) DO UPDATE
    SET available_at = media_replicas.available_at
    RETURNING node_id, media_object_id, available_at
)
SELECT n.public_id, m.public_id, u.available_at
FROM upserted u
JOIN nodes n ON n.id = u.node_id
JOIN media_objects m ON m.id = u.media_object_id`, nodeID, mediaID, size, infoHashV2).
		Scan(&replica.NodeID, &replica.MediaID, &replica.AvailableAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.MediaReplica{}, store.ErrReplicaNotEligible
	}
	if err != nil {
		return store.MediaReplica{}, fmt.Errorf("report media replica: %w", err)
	}
	return replica, nil
}

func (s *Store) GetMediaReplica(ctx context.Context, nodeID, mediaID string) (store.MediaReplica, error) {
	if !nodes.ValidPublicID(nodeID) {
		return store.MediaReplica{}, nodes.ErrInvalidPublicID
	}
	if !media.ValidMediaID(mediaID) {
		return store.MediaReplica{}, media.ErrInvalidMediaID
	}
	var replica store.MediaReplica
	err := s.db.QueryRowContext(ctx, `
SELECT n.public_id, m.public_id, r.available_at
FROM media_replicas r
JOIN nodes n ON n.id = r.node_id
JOIN media_objects m ON m.id = r.media_object_id
WHERE n.public_id = $1 AND m.public_id = $2`, nodeID, mediaID).
		Scan(&replica.NodeID, &replica.MediaID, &replica.AvailableAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.MediaReplica{}, store.ErrNotFound
	}
	if err != nil {
		return store.MediaReplica{}, fmt.Errorf("get media replica: %w", err)
	}
	return replica, nil
}

// MediaSourceEligible reports whether nodeID is currently an eligible peer
// source for mediaID (same predicate as ListEligibleMediaSources).
func (s *Store) MediaSourceEligible(ctx context.Context, nodeID, mediaID string) (bool, error) {
	if !media.ValidMediaID(mediaID) {
		return false, media.ErrInvalidMediaID
	}
	var eligible bool
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
  SELECT 1 FROM media_replicas r
  JOIN nodes n ON n.id = r.node_id
  JOIN media_objects m ON m.id = r.media_object_id
  WHERE n.public_id = $1 AND m.public_id = $2
    AND m.state = 'ACTIVE'
    AND n.enrollment_status = 'APPROVED'
    AND n.trust_status = 'TRUSTED'
    AND n.peer_serving = true)`, nodeID, mediaID).Scan(&eligible)
	if err != nil {
		return false, fmt.Errorf("media source eligibility: %w", err)
	}
	return eligible, nil
}

func (s *Store) ListEligibleMediaSources(ctx context.Context, mediaID string, limit int) ([]store.MediaReplica, error) {
	if !media.ValidMediaID(mediaID) {
		return nil, media.ErrInvalidMediaID
	}
	if limit < 1 || limit > 100 {
		return nil, media.ErrInvalidLimit
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT n.public_id, m.public_id, r.available_at
FROM media_replicas r
JOIN nodes n ON n.id = r.node_id
JOIN media_objects m ON m.id = r.media_object_id
WHERE m.public_id = $1
  AND m.state = 'ACTIVE'
  AND n.enrollment_status = 'APPROVED'
  AND n.trust_status = 'TRUSTED'
  AND n.peer_serving = true
ORDER BY n.public_id
LIMIT $2`, mediaID, limit)
	if err != nil {
		return nil, fmt.Errorf("list eligible media sources: %w", err)
	}
	defer rows.Close()
	sources := make([]store.MediaReplica, 0)
	for rows.Next() {
		var source store.MediaReplica
		if err := rows.Scan(&source.NodeID, &source.MediaID, &source.AvailableAt); err != nil {
			return nil, fmt.Errorf("list eligible media sources: %w", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list eligible media sources: %w", err)
	}
	return sources, nil
}

func validReplicaInfoHashV2(value string) bool {
	if len(value) != media.InfoHashV2Length {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
