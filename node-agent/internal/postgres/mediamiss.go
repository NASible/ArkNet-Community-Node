package postgres

import (
	"context"
	"fmt"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/mediamiss"
)

var _ mediamiss.Repository = (*Store)(nil)

// MediaMissPolicy defaults to ASK for fresh and upgraded Nodes.
func (s *Store) MediaMissPolicy(ctx context.Context) (mediamiss.Policy, error) {
	var policy string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(settings->>'media_miss_policy', $1)
FROM node_configuration WHERE singleton = true`, string(mediamiss.Default)).Scan(&policy); err != nil {
		return "", fmt.Errorf("read media miss policy: %w", err)
	}
	if !mediamiss.ValidPolicy(mediamiss.Policy(policy)) {
		return "", fmt.Errorf("read media miss policy: invalid stored value")
	}
	return mediamiss.Policy(policy), nil
}

func (s *Store) SetMediaMissPolicy(ctx context.Context, policy mediamiss.Policy) error {
	if !mediamiss.ValidPolicy(policy) {
		return fmt.Errorf("save media miss policy: invalid policy")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE node_configuration
SET settings = jsonb_set(settings, '{media_miss_policy}', to_jsonb($1::text), true)
WHERE singleton = true`, string(policy))
	if err != nil {
		return fmt.Errorf("save media miss policy: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("save media miss policy: configuration unavailable")
	}
	return nil
}

func (s *Store) StoreMediaMisses(ctx context.Context, notifications []mediamiss.Notification) error {
	if len(notifications) > catalog.MaxMisses {
		return fmt.Errorf("store media misses: too many notifications")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, n := range notifications {
		if !selectionIDPattern.MatchString(n.MediaID) || n.Size < 0 || n.MissCount < 1 {
			return fmt.Errorf("store media misses: invalid notification")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO media_miss_notifications (media_id, title, size, miss_count)
VALUES ($1, $2, $3, $4)
ON CONFLICT (media_id) DO UPDATE
SET title = EXCLUDED.title, size = EXCLUDED.size, miss_count = EXCLUDED.miss_count, updated_at = clock_timestamp()`,
			n.MediaID, n.Title, n.Size, n.MissCount); err != nil {
			return fmt.Errorf("store media misses: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) PendingMediaMisses(ctx context.Context) ([]mediamiss.Notification, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.media_id, n.title, n.size, n.miss_count
FROM media_miss_notifications n
WHERE n.state = 'PENDING'
  AND NOT EXISTS (SELECT 1 FROM media_selections s WHERE s.id = n.media_id AND s.local_state <> 'NOT_PRESENT')
ORDER BY n.miss_count DESC, n.media_id
LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("list media misses: %w", err)
	}
	defer rows.Close()
	result := []mediamiss.Notification{}
	for rows.Next() {
		var n mediamiss.Notification
		if err := rows.Scan(&n.MediaID, &n.Title, &n.Size, &n.MissCount); err != nil {
			return nil, fmt.Errorf("list media misses: %w", err)
		}
		result = append(result, n)
	}
	return result, rows.Err()
}

func (s *Store) IgnoreMediaMiss(ctx context.Context, mediaID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE media_miss_notifications SET state = 'IGNORED', updated_at = clock_timestamp()
WHERE media_id = $1 AND state = 'PENDING'`, mediaID)
	if err != nil {
		return fmt.Errorf("ignore media miss: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return mediamiss.ErrNotFound
	}
	return nil
}
