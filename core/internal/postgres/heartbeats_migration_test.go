package postgres

import (
	"arknet/core/internal/nodes"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"reflect"
	"testing"
	"testing/fstest"
	"time"
)

func TestHeartbeatMigrationUpgradesExistingMetadataAndRollsBack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	old := fstest.MapFS{}
	for _, name := range []string{"0001_nodes.sql", "0002_node_credentials.sql"} {
		raw, err := fs.ReadFile(migrationFiles, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		old["migrations/"+name] = &fstest.MapFile{Data: raw}
	}
	if err := migrate(ctx, s.db, old); err != nil {
		t.Fatal(err)
	}
	id, _ := nodes.NewPublicID()
	key := bytes.Repeat([]byte{77}, 32)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO nodes(public_id,software_version,enrollment_public_key,last_seen_at) VALUES($1,'old',$2,clock_timestamp())", id, key); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := s.db.QueryRowContext(ctx, "SELECT last_seen_at FROM nodes WHERE public_id=$1", id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	raw, err := fs.ReadFile(migrationFiles, "migrations/0003_node_heartbeats.sql")
	if err != nil {
		t.Fatal(err)
	}
	old["migrations/0003_node_heartbeats.sql"] = &fstest.MapFile{Data: raw}
	old["migrations/0004_invalid.sql"] = &fstest.MapFile{Data: []byte("THIS IS NOT SQL")}
	if err := migrate(ctx, s.db, old); err == nil {
		t.Fatal("invalid migration committed")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM arknet_schema_migrations").Scan(&count); err != nil || count != 2 {
		t.Fatalf("rollback ledger=%d err=%v", count, err)
	}
	if _, err := s.db.ExecContext(ctx, "SELECT current_state FROM nodes"); err == nil {
		t.Fatal("migration column not rolled back")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	node, stored, err := s.EnrollmentIdentity(ctx, id)
	if err != nil || !bytes.Equal(key, stored) || node.SoftwareVersion != "old" || node.CurrentState != nil || node.ReportedHealth != nil || node.LastSeenAt == nil || !node.LastSeenAt.Equal(before) {
		t.Fatalf("upgrade lost metadata %+v %v", node, err)
	}
	old["migrations/0003_node_heartbeats.sql"] = &fstest.MapFile{Data: append(append([]byte(nil), raw...), []byte("\n-- drift")...)}
	delete(old, "migrations/0004_invalid.sql")
	if err := migrate(ctx, s.db, old); !errors.Is(err, ErrMigrationDrift) {
		t.Fatalf("checksum drift %v", err)
	}
}
func TestHeartbeatDatabaseRejectsNonminimalMetadata(t *testing.T) {
	s, _, id, _ := heartbeatStore(t)
	ctx := context.Background()
	for _, health := range []string{`null`, `{}`, `[]`, `{"healthy":true}`, `{"healthy":true,"database_healthy":"true"}`, `{"healthy":true,"database_healthy":false,"private":"x"}`} {
		if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET reported_health=$2::jsonb WHERE public_id=$1", id, health); err == nil {
			t.Fatalf("accepted health %s", health)
		}
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET current_state='operator@example.com' WHERE public_id=$1", id); err == nil {
		t.Fatal("accepted invalid state")
	}
}
func TestHeartbeatFailedWriteRollsBackAllMetadata(t *testing.T) {
	s, repo, id, key := heartbeatStore(t)
	ctx := context.Background()
	before, _ := s.GetByPublicID(ctx, id)
	if _, err := s.db.ExecContext(ctx, `CREATE FUNCTION fail_heartbeat() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'secret test error'; END $$; CREATE TRIGGER fail_heartbeat BEFORE UPDATE ON nodes FOR EACH ROW EXECUTE FUNCTION fail_heartbeat()`); err != nil {
		t.Fatal(err)
	}
	proof := time.Now().Unix()
	if err := repo.AcceptHeartbeat(ctx, id, "v2", "MESH", true, true, false, key, proof); err == nil {
		t.Fatal("failed update succeeded")
	}
	after, _ := s.GetByPublicID(ctx, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("partial metadata after rollback")
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_heartbeat ON nodes"); err != nil {
		t.Fatal(err)
	}
	if err := repo.AcceptHeartbeat(ctx, id, "v2", "MESH", true, true, false, key, proof); err != nil {
		t.Fatalf("failed request consumed proof %v", err)
	}
}
func TestHeartbeatReplayProtectionSurvivesReopen(t *testing.T) {
	s, repo, id, key := heartbeatStore(t)
	ctx := context.Background()
	proof := time.Now().Unix()
	if err := repo.AcceptHeartbeat(ctx, id, "v2", "CONNECTED", true, false, false, key, proof); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetByPublicID(ctx, id)
	var schema string
	if err := s.db.QueryRowContext(ctx, "SHOW search_path").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("ARKNET_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.AcceptHeartbeat(ctx, id, "replay", "OFFLINE", false, true, false, key, proof); !errors.Is(err, nodes.ErrHeartbeatReplay) {
		t.Fatalf("reopen replay %v", err)
	}
	after, _ := reopened.GetByPublicID(ctx, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reopened replay changed metadata")
	}
}
