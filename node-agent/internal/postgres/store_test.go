package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"regexp"
	"testing"
	"time"

	"arknet/node-agent/internal/localstate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ARKNET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ARKNET_TEST_DATABASE_URL for isolated-schema PostgreSQL integration tests")
	}
	return dsn
}

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("ARKNET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ARKNET_TEST_DATABASE_URL for isolated-schema PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	admin := stdlib.OpenDB(*cfg)
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	schema := "agent_test_" + hex.EncodeToString(random)
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer admin.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg.RuntimeParams["search_path"] = schema
	// ConnString returns the original input; modifying RuntimeParams does not
	// rewrite it. Open the test pool from the modified config itself.
	store := &Store{db: stdlib.OpenDB(*cfg)}
	if err := store.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestInvalidStoredIdentityIsRejectedWithoutRepair(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"operator@example.com", "node_gggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg", "node_" + "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCD", "node_123"} {
		if _, err := s.db.ExecContext(ctx, "UPDATE node_identity SET public_id = $1", invalid); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, "DELETE FROM node_configuration"); err != nil {
			t.Fatal(err)
		}
		if err := s.Initialize(ctx); err == nil {
			t.Fatal("invalid stored identity was accepted")
		}
		if _, err := s.Snapshot(ctx); err == nil {
			t.Fatal("snapshot exposed invalid identity")
		}
		var id string
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT public_id FROM node_identity").Scan(&id); err != nil || id != invalid {
			t.Fatalf("identity changed: %q, %v", id, err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM node_configuration").Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed initialization committed defaults: %d, %v", count, err)
		}
	}
}

func TestMissingIdentityWithExistingRowsFailsClosed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM node_identity"); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx); err == nil {
		t.Fatal("missing identity was silently regenerated despite existing local rows")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM node_identity").Scan(&count); err != nil || count != 0 {
		t.Fatalf("identity count = %d, %v", count, err)
	}
}

func TestInitializeDefaultsAndReplay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^node_[0-9a-f]{64}$`).MatchString(first.NodeID) || first.State != localstate.Provisioning {
		t.Fatalf("unexpected snapshot: %+v", first)
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := s.Snapshot(ctx)
	if err != nil || first != second {
		t.Fatalf("replay = %+v, %v", second, err)
	}
	var migrationCount int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM arknet_schema_migrations").Scan(&migrationCount); err != nil || migrationCount != 10 {
		t.Fatalf("migration count = %d, %v", migrationCount, err)
	}
	var settings, status string
	var coreURL, enrolledAt sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT settings::text FROM node_configuration").Scan(&settings); err != nil || settings != "{}" {
		t.Fatalf("settings = %q, %v", settings, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT status, core_url, enrolled_at::text FROM core_enrollment").Scan(&status, &coreURL, &enrolledAt); err != nil || status != "NOT_ENROLLED" || coreURL.Valid || enrolledAt.Valid {
		t.Fatalf("enrollment = %q, %v, %v, %v", status, coreURL, enrolledAt, err)
	}
	for _, table := range []string{"node_identity", "node_configuration", "node_state", "core_enrollment"} {
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count = %d, %v", table, count, err)
		}
	}
}

func TestPeerServingDefaultsOffAndPersists(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if enabled, err := s.PeerServing(ctx); err != nil || enabled {
		t.Fatalf("fresh peer serving = %v, %v", enabled, err)
	}
	if err := s.SetPeerServing(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if enabled, err := s.PeerServing(ctx); err != nil || !enabled {
		t.Fatalf("persisted peer serving = %v, %v", enabled, err)
	}
	if err := s.SetPeerServing(ctx, false); err != nil {
		t.Fatal(err)
	}
	if enabled, err := s.PeerServing(ctx); err != nil || enabled {
		t.Fatalf("disabled peer serving = %v, %v", enabled, err)
	}
}
