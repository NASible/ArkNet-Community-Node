package postgres

import (
	"context"
	"fmt"
)

// PeerServing reads the operator's local opt-in. An absent value is the safe
// default for fresh and upgraded nodes.
func (s *Store) PeerServing(ctx context.Context) (bool, error) {
	var enabled bool
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE((settings->>'peer_serving')::boolean, false)
FROM node_configuration WHERE singleton = true`).Scan(&enabled); err != nil {
		return false, fmt.Errorf("read peer serving setting: %w", err)
	}
	return enabled, nil
}

func (s *Store) SetPeerServing(ctx context.Context, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE node_configuration
SET settings = jsonb_set(settings, '{peer_serving}', to_jsonb($1::boolean), true)
WHERE singleton = true`, enabled)
	if err != nil {
		return fmt.Errorf("save peer serving setting: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return fmt.Errorf("save peer serving setting: configuration unavailable")
	}
	return nil
}
