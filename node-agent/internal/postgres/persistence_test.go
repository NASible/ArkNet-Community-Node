package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"arknet/node-agent/internal/localstate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestPersistedRowsSurviveReopen(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := s.EnrollmentIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE node_configuration SET settings = '{"retained":true}'`,

		`UPDATE core_enrollment SET status = 'CUSTOM_STATUS', core_url = 'https://core.example.test', enrolled_at = '2026-01-01T00:00:00Z'`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	first, err = s.Transition(ctx, localstate.Provisioning, localstate.EnrollmentPending)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := s.db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := &Store{db: stdlib.OpenDB(*cfg)}
	defer reopened.Close()
	if err := reopened.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	restoredCredential, err := reopened.EnrollmentIdentity(ctx)
	if err != nil || !bytes.Equal(credential.PrivateSeed, restoredCredential.PrivateSeed) || credential.NodeID != restoredCredential.NodeID {
		t.Fatal("credential changed after reopen")
	}
	snapshot, err := reopened.Snapshot(ctx)
	if err != nil || snapshot != first {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	if historyCount(t, reopened) != 1 {
		t.Fatal("transition history lost on reopen")
	}
	var retained bool
	if err := reopened.db.QueryRowContext(ctx, `SELECT settings = '{"retained":true}'::jsonb FROM node_configuration`).Scan(&retained); err != nil || !retained {
		t.Fatalf("configuration reset: %v", err)
	}
	if err := reopened.db.QueryRowContext(ctx, `SELECT status = 'CUSTOM_STATUS' AND core_url = 'https://core.example.test' AND enrolled_at = '2026-01-01T00:00:00Z'::timestamptz FROM core_enrollment`).Scan(&retained); err != nil || !retained {
		t.Fatalf("enrollment reset: %v", err)
	}
}

func TestConcurrentInitializationUsesOneIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var schema string
	if err := s.db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	other := &Store{db: stdlib.OpenDB(*cfg)}
	defer other.Close()
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	ids := make(chan string, 12)
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		target := s
		if i%2 != 0 {
			target = other
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := target.Initialize(ctx); err != nil {
				failures <- err
				return
			}
			snapshot, err := target.Snapshot(ctx)
			if err != nil {
				failures <- err
				return
			}
			ids <- snapshot.NodeID
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	close(ids)
	for err := range failures {
		t.Error(err)
	}
	var identity string
	var count int
	for id := range ids {
		if identity != "" && id != identity {
			t.Fatal("startup generated multiple identities")
		}
		identity = id
		count++
	}
	var credentialCount int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM node_federation_credential").Scan(&credentialCount); err != nil || credentialCount != 1 {
		t.Fatal("concurrent initialization created invalid credential count")
	}
	if count != 12 {
		t.Fatalf("successful initializations = %d", count)
	}
}

func TestMissingRowsRestoredWithoutChangingIdentity(t *testing.T) {
	for _, table := range []string{"node_configuration", "node_state", "core_enrollment"} {
		t.Run(table, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			if err := s.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			first, err := s.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				t.Fatal(err)
			}
			if err := s.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			restored, err := s.Snapshot(ctx)
			if err != nil || restored.NodeID != first.NodeID || restored.State != first.State || (table != "node_state" && !restored.StateChangedAt.Equal(first.StateChangedAt)) || (table == "node_state" && !restored.StateChangedAt.After(first.StateChangedAt)) {
				t.Fatalf("restored = %+v, %v", restored, err)
			}
			var count int
			if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
				t.Fatalf("restored count = %d, %v", count, err)
			}
		})
	}
}

func TestInitializationMigrationFailureIsAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE node_state (probe text)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx); err == nil {
		t.Fatal("expected conflicting migration error")
	}
	for _, table := range []string{"node_identity", "node_configuration", "core_enrollment", "arknet_schema_migrations"} {
		var name sql.NullString
		if err := s.db.QueryRowContext(ctx, "SELECT to_regclass($1)::text", table).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name.Valid {
			t.Fatalf("failed initialization left %s", table)
		}
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE node_state"); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestInitializationDefaultsFailureIsAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"DELETE FROM node_configuration", "DELETE FROM node_state", "DELETE FROM core_enrollment",
		"ALTER TABLE core_enrollment ADD CONSTRAINT reject_defaults CHECK (status <> 'NOT_ENROLLED')",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Initialize(ctx); err == nil {
		t.Fatal("expected defaults constraint failure")
	}
	for _, table := range []string{"node_configuration", "node_state", "core_enrollment"} {
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed initialization committed %s: %d, %v", table, count, err)
		}
	}
	var id string
	if err := s.db.QueryRowContext(ctx, "SELECT public_id FROM node_identity").Scan(&id); err != nil || id != first.NodeID {
		t.Fatalf("identity = %q, %v", id, err)
	}
}

func TestMigrationChecksumDriftIsRejected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE arknet_schema_migrations SET checksum = 'tampered'"); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx); !errors.Is(err, ErrMigrationDrift) {
		t.Fatalf("error = %v", err)
	}
}

func TestFailedMigrationRollsBackDDLAndLedger(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	broken := fstest.MapFS{
		"migrations/0001_probe.sql":  {Data: []byte("CREATE TABLE rolled_back (id integer);")},
		"migrations/0002_broken.sql": {Data: []byte("INVALID SQL;")},
	}
	if err := migrate(ctx, tx, broken); err == nil {
		tx.Rollback()
		t.Fatal("expected migration error")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"rolled_back", "arknet_schema_migrations"} {
		var name sql.NullString
		if err := s.db.QueryRowContext(ctx, "SELECT to_regclass($1)::text", table).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name.Valid {
			t.Fatalf("failed migration left %s", table)
		}
	}
}

func TestSingletonTablesHaveOnlyExpectedColumns(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"node_identity": "singleton,public_id", "node_configuration": "singleton,settings",
		"node_state": "singleton,state,changed_at", "core_enrollment": "singleton,status,core_url,enrolled_at,last_successful_core_contact_at",
	}
	for table, columns := range expected {
		var actual string
		if err := s.db.QueryRowContext(ctx, `SELECT string_agg(column_name, ',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&actual); err != nil || actual != columns {
			t.Fatalf("%s columns = %q, %v", table, actual, err)
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE "+table+" SET singleton = false"); err == nil {
			t.Fatalf("%s accepted a second singleton key", table)
		}
	}
}

func TestOpenRejectsSecretBearingInvalidDSN(t *testing.T) {
	secret := "SECRET_SHOULD_NOT_LEAK"
	s, err := Open(context.Background(), "postgres://user:"+secret+"@localhost:invalid/db")
	if err == nil || s != nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe parse error: %v", err)
	}
}
