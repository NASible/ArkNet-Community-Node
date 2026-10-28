package postgres

import (
	"context"
	"testing"
)

func TestAutomaticUpdatesDefaultOnAndOptOutPersists(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if enabled, err := s.AutomaticUpdates(ctx); err != nil || !enabled {
		t.Fatalf("default = %v %v", enabled, err)
	}
	if err := s.SetAutomaticUpdates(ctx, false); err != nil {
		t.Fatal(err)
	}
	// Re-running migrations (Agent restart) keeps the operator's choice.
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := s.AutomaticUpdates(ctx); enabled {
		t.Fatal("opt-out lost")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE node_configuration SET settings = jsonb_set(settings, '{automatic_updates}', '"false"')`); err == nil {
		t.Fatal("schema accepted a non-boolean automatic_updates")
	}
	if err := s.SetAutomaticUpdates(ctx, true); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := s.AutomaticUpdates(ctx); !enabled {
		t.Fatal("re-enable lost")
	}
}
