package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"arknet/core/internal/relayrevocations"
)

// PendingRelayRevocations lists (Node, relay) deliveries not yet acknowledged,
// oldest decision first.
func (s *Store) PendingRelayRevocations(ctx context.Context, relayIDs []string, limit int) ([]relayrevocations.Task, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid limit")
	}
	if len(relayIDs) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.public_id, r.relay_id, e.decided_at, COALESCE(d.attempts, 0), COALESCE(d.last_error, ''), d.last_attempt_at
		FROM relay_revocation_events e
		JOIN nodes n ON n.id = e.node_id
		CROSS JOIN unnest($1::text[]) AS r(relay_id)
		LEFT JOIN relay_revocation_deliveries d ON d.node_id = e.node_id AND d.relay_id = r.relay_id
		WHERE d.acknowledged_at IS NULL
		ORDER BY e.decided_at, n.public_id, r.relay_id
		LIMIT $2`, relayIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("list relay revocations: %w", err)
	}
	defer rows.Close()
	var out []relayrevocations.Task
	for rows.Next() {
		var task relayrevocations.Task
		var last sql.NullTime
		if err := rows.Scan(&task.NodeID, &task.RelayID, &task.DecidedAt, &task.Attempts, &task.LastError, &last); err != nil {
			return nil, fmt.Errorf("scan relay revocation: %w", err)
		}
		if last.Valid {
			task.LastAttemptAt = last.Time
		}
		out = append(out, task)
	}
	return out, rows.Err()
}

// RecordRelayRevocationAttempt counts one attempt; an empty failure marks the
// delivery acknowledged.
func (s *Store) RecordRelayRevocationAttempt(ctx context.Context, nodeID, relayID, failure string) error {
	var acknowledged any
	if failure == "" {
		acknowledged = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO relay_revocation_deliveries (node_id, relay_id, attempts, last_error, last_attempt_at, acknowledged_at)
		SELECT e.node_id, $2, 1, NULLIF($3, ''), now(), $4
		FROM relay_revocation_events e JOIN nodes n ON n.id = e.node_id WHERE n.public_id = $1
		ON CONFLICT (node_id, relay_id) DO UPDATE SET attempts = relay_revocation_deliveries.attempts + 1,
			last_error = EXCLUDED.last_error, last_attempt_at = EXCLUDED.last_attempt_at,
			acknowledged_at = COALESCE(relay_revocation_deliveries.acknowledged_at, EXCLUDED.acknowledged_at)`,
		nodeID, relayID, failure, acknowledged)
	if err != nil {
		return fmt.Errorf("record relay revocation attempt: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return errors.New("no revocation for this Node")
	}
	return nil
}

// MediaInfoHashes lists the info hash of every catalog object, removed ones included.
func (s *Store) MediaInfoHashes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT info_hash_v2 FROM media_objects WHERE info_hash_v2 IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list info hashes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		out = append(out, hash)
	}
	return out, rows.Err()
}
