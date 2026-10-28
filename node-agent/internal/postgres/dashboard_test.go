package postgres

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func reopenTestStore(t *testing.T, schema string) *Store {
	t.Helper()
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	s := &Store{db: stdlib.OpenDB(*cfg)}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigration0004UpgradesExistingEnrollmentWithoutDataLoss(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	legacy := fstest.MapFS{}
	for _, name := range []string{"0001_local_state.sql", "0002_state_machine.sql", "0003_federation_credential.sql"} {
		data, err := fs.ReadFile(migrationFiles, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		legacy["migrations/"+name] = &fstest.MapFile{Data: data}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, tx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO node_identity(singleton,public_id) VALUES(true,'node_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')`,
		`INSERT INTO node_configuration(singleton,settings) VALUES(true,'{}')`,
		`INSERT INTO node_state(singleton,state) VALUES(true,'CONNECTED')`,
		`INSERT INTO node_federation_credential(singleton,node_public_id,private_seed) VALUES(true,'node_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',decode(repeat('00',32),'hex'))`,
		`INSERT INTO core_enrollment(singleton,status,core_url,enrolled_at) VALUES(true,'APPROVED','https://core.example.test','2026-01-01T00:00:00Z')`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	var status, coreURL string
	var enrolledAt time.Time
	var contact sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT status,core_url,enrolled_at,last_successful_core_contact_at FROM core_enrollment`).Scan(&status, &coreURL, &enrolledAt, &contact); err != nil {
		t.Fatal(err)
	}
	if status != "APPROVED" || coreURL != "https://core.example.test" || !enrolledAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || contact.Valid {
		t.Fatalf("upgrade changed enrollment: %q %q %v %v", status, coreURL, enrolledAt, contact)
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("migration replay failed: %v", err)
	}
}

func TestSuccessfulCoreContactUsesDatabaseClockAndSurvivesReopen(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE core_enrollment SET status='APPROVED'`); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	contact, err := s.RecordSuccessfulCoreContact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if contact.Before(before) || contact.After(after) {
		t.Fatalf("contact %v is outside database clock interval [%v,%v]", contact, before, after)
	}
	var schema string
	if err := s.db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	reopened := reopenTestStore(t, schema)
	if err := reopened.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.EnrollmentState != "APPROVED" || !snapshot.LastSuccessfulCoreContactAt.Equal(contact) {
		t.Fatalf("contact did not survive restart: %+v", snapshot)
	}
}

func TestSuccessfulCoreContactRequiresApprovedEnrollment(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSuccessfulCoreContact(ctx); err == nil {
		t.Fatal("recorded contact for unapproved enrollment")
	}
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.LastSuccessfulCoreContactAt.IsZero() {
		t.Fatalf("unexpected contact: %v", snapshot.LastSuccessfulCoreContactAt)
	}
}
