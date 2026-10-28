package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var ErrMigrationDrift = errors.New("applied migration differs from this release")

func (s *Store) Migrate(ctx context.Context) error { return migrate(ctx, s.db, migrationFiles) }

// Migrations and their ledger entries commit together. A transaction-scoped
// advisory lock serializes concurrent Core startups against the same database.
func migrate(ctx context.Context, db *sql.DB, files fs.FS) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('arknet-core-schema'))"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS arknet_schema_migrations (
        version bigint PRIMARY KEY,
        name text NOT NULL,
        checksum text NOT NULL,
        applied_at timestamptz NOT NULL DEFAULT now()
    )`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		name := entry.Name()
		prefix, _, found := strings.Cut(name, "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil || !found || version <= 0 {
			return fmt.Errorf("invalid migration filename: %s", name)
		}
		data, err := fs.ReadFile(files, "migrations/"+name)
		if err != nil {
			return err
		}
		checksum := fmt.Sprintf("%x", sha256.Sum256(data))
		var recordedName, recordedChecksum string
		err = tx.QueryRowContext(ctx, "SELECT name, checksum FROM arknet_schema_migrations WHERE version = $1", version).Scan(&recordedName, &recordedChecksum)
		if err == nil {
			if recordedName != name || recordedChecksum != checksum {
				return fmt.Errorf("%w: %s", ErrMigrationDrift, name)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(data)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO arknet_schema_migrations (version, name, checksum) VALUES ($1, $2, $3)", version, name, checksum); err != nil {
			return err
		}
	}
	return tx.Commit()
}
