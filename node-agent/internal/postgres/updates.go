package postgres

import (
	"context"
	"fmt"
)

// AutomaticUpdates defaults to ON for fresh and upgraded Nodes.
func (s *Store) AutomaticUpdates(ctx context.Context) (bool, error) {
	var enabled bool
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE((settings->'automatic_updates')::boolean, true)
FROM node_configuration WHERE singleton = true`).Scan(&enabled); err != nil {
		return false, fmt.Errorf("read automatic updates: %w", err)
	}
	return enabled, nil
}

func (s *Store) SetAutomaticUpdates(ctx context.Context, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE node_configuration
SET settings = jsonb_set(settings, '{automatic_updates}', to_jsonb($1::boolean), true)
WHERE singleton = true`, enabled)
	if err != nil {
		return fmt.Errorf("save automatic updates: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("save automatic updates: configuration unavailable")
	}
	return nil
}
