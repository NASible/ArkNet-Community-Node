package postgres

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"arknet/core/internal/nodes"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("ARKNET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ARKNET_TEST_DATABASE_URL or run scripts/check-core-stack.py for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("open test database failed")
	}
	id, err := nodes.NewPublicID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "arknet_test_" + id[5:21]
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer admin.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	store, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestCreateAndReadNode(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(ctx, "0.0.1-dev")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID <= 0 || !nodes.ValidPublicID(created.PublicID) || created.CreatedAt.IsZero() {
		t.Fatalf("invalid record: %+v", created)
	}
	if created.EnrollmentStatus != nodes.EnrollmentPending || created.TrustStatus != nodes.TrustUntrusted || created.LastSeenAt != nil {
		t.Fatalf("unsafe initial state: %+v", created)
	}
	read, err := store.GetByPublicID(ctx, created.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if read.ID != created.ID || read.PublicID != created.PublicID || read.SoftwareVersion != "0.0.1-dev" || !read.CreatedAt.Equal(created.CreatedAt) || read.EnrollmentStatus != created.EnrollmentStatus || read.TrustStatus != created.TrustStatus || read.LastSeenAt != nil {
		t.Fatalf("round trip mismatch: %+v", read)
	}
	other, err := store.Create(ctx, "0.0.2-dev")
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == created.ID || other.PublicID == created.PublicID {
		t.Fatal("duplicate node identifiers")
	}
	missingID, _ := nodes.NewPublicID()
	if _, err := store.GetByPublicID(ctx, missingID); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("not-found error = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO nodes (public_id, software_version) VALUES ($1, $2)", created.PublicID, "test"); err == nil {
		t.Fatal("duplicate public ID was accepted")
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO nodes (public_id, software_version) VALUES ($1, $2)", "operator@example.com", "test"); err == nil {
		t.Fatal("identifying public ID was accepted")
	}
}

func TestInvalidInputsDoNotAccessDatabase(t *testing.T) {
	store := &Store{}
	if _, err := store.Create(context.Background(), " \t"); !errors.Is(err, nodes.ErrInvalidSoftwareVersion) {
		t.Fatalf("error = %v", err)
	}
	if _, err := store.GetByPublicID(context.Background(), "operator@example.com"); !errors.Is(err, nodes.ErrInvalidPublicID) {
		t.Fatalf("error = %v", err)
	}
}

func TestMigrationsReplayAndConcurrentStartup(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errors <- store.Migrate(ctx) }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	created, err := store.Create(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM arknet_schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if count != len(entries) {
		t.Fatalf("ledger entries = %d, migration files = %d", count, len(entries))
	}
	if _, err := store.GetByPublicID(ctx, created.PublicID); err != nil {
		t.Fatal("migration replay lost node: ", err)
	}
}

func TestMigrationChecksumDriftIsRejected(t *testing.T) {
	store := testStore(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	original, err := migrationFiles.ReadFile("migrations/0001_nodes.sql")
	if err != nil {
		t.Fatal(err)
	}
	changed := fstest.MapFS{"migrations/0001_nodes.sql": &fstest.MapFile{Data: append(original, []byte("\n-- changed")...)}}
	if err := migrate(context.Background(), store.db, changed); !errors.Is(err, ErrMigrationDrift) {
		t.Fatalf("error = %v", err)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	store := testStore(t)
	broken := fstest.MapFS{
		"migrations/0001_probe.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE rolled_back (id integer);")},
		"migrations/0002_broken.sql": &fstest.MapFile{Data: []byte("INVALID SQL;")},
	}
	if err := migrate(context.Background(), store.db, broken); err == nil {
		t.Fatal("expected migration error")
	}
	for _, table := range []string{"rolled_back", "arknet_schema_migrations"} {
		var name sql.NullString
		if err := store.db.QueryRow("SELECT to_regclass($1)::text", table).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name.Valid {
			t.Fatalf("failed transaction left %s", table)
		}
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
