package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/core/internal/users"
)

const testHash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g"

func migratedStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUsersStoreOnlyOpaqueIDsAndHashes(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	user, err := s.CreateUser(ctx, "alice", testHash)
	if err != nil {
		t.Fatal(err)
	}
	if !users.ValidUserID(user.ID) || user.Username != "alice" || user.Status != users.StatusActive || user.CreatedAt.IsZero() {
		t.Fatalf("user %+v", user)
	}
	if _, err := s.CreateUser(ctx, "alice", testHash); !errors.Is(err, users.ErrUsernameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.CreateUser(ctx, "Alice", testHash); !errors.Is(err, users.ErrInvalidUsername) {
		t.Fatalf("non-normalized: %v", err)
	}
	if _, err := s.CreateUser(ctx, "bob", "plaintext-password"); err == nil {
		t.Fatal("schema accepted a non-argon2id password value")
	}
	credentials, err := s.GetCredentials(ctx, "alice")
	if err != nil || credentials.UserID != user.ID || credentials.PasswordHash != testHash || credentials.Status != users.StatusActive {
		t.Fatalf("credentials %+v %v", credentials, err)
	}
	if _, err := s.GetCredentials(ctx, "nobody"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	for _, statement := range []string{
		"INSERT INTO global_users (public_id, username, password_hash) VALUES ('alice', 'carol', '" + testHash + "')",
		"UPDATE global_users SET status = 'deleted' WHERE username = 'alice'",
		"INSERT INTO global_user_sessions (token_digest, user_id, expires_at) SELECT '\\x00'::bytea, id, now() + interval '1 hour' FROM global_users",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted %q", statement)
		}
	}
}

func TestSessionsExpireAndRequireActiveUser(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	user, err := s.CreateUser(ctx, "alice", testHash)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, _ := users.NewSessionToken()
	expiresAt, err := s.CreateSession(ctx, user.ID, digest, users.SessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if until := time.Until(expiresAt); until < 11*time.Hour || until > 13*time.Hour {
		t.Fatalf("expires in %s", until)
	}
	session, err := s.GetSession(ctx, digest)
	if err != nil || session.UserID != user.ID || !session.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("session %+v %v", session, err)
	}

	_, expired, _ := users.NewSessionToken()
	if _, err := s.CreateSession(ctx, user.ID, expired, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE global_user_sessions SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE token_digest = $1", expired[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, expired); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("expired session: %v", err)
	}
	_, third, _ := users.NewSessionToken()
	if _, err := s.CreateSession(ctx, user.ID, third, time.Hour); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM global_user_sessions WHERE token_digest = $1", expired[:]).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("expired session not pruned: %d %v", remaining, err)
	}

	if err := s.DeleteSession(ctx, third); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, third); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("deleted session: %v", err)
	}

	disabled, err := s.SetUserStatus(ctx, user.ID, users.StatusDisabled)
	if err != nil || disabled.Status != users.StatusDisabled {
		t.Fatalf("disable %+v %v", disabled, err)
	}
	if _, err := s.GetSession(ctx, digest); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("disabled user's session survived: %v", err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM global_user_sessions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("sessions not revoked: %d %v", count, err)
	}
	_, fresh, _ := users.NewSessionToken()
	if _, err := s.CreateSession(ctx, user.ID, fresh, time.Hour); !errors.Is(err, users.ErrInactive) {
		t.Fatalf("disabled user got session: %v", err)
	}
	if _, err := s.SetUserStatus(ctx, user.ID, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, digest); !errors.Is(err, users.ErrNotFound) {
		t.Fatal("re-enabling resurrected a revoked session")
	}
	if _, err := s.CreateSession(ctx, user.ID, fresh, time.Hour); err != nil {
		t.Fatalf("re-enabled user: %v", err)
	}
	for _, test := range []struct {
		id     string
		status users.Status
		want   error
	}{
		{"user_" + strings.Repeat("0", 64), users.StatusDisabled, users.ErrNotFound},
		{"node_" + strings.Repeat("0", 64), users.StatusDisabled, users.ErrInvalidUserID},
		{user.ID, "DELETED", users.ErrInvalidStatus},
	} {
		if _, err := s.SetUserStatus(ctx, test.id, test.status); !errors.Is(err, test.want) {
			t.Fatalf("%s %s: %v", test.id, test.status, err)
		}
	}
}

// A disable racing session creation must never leave a usable session.
func TestConcurrentDisableNeverLeavesActiveSession(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		user, err := s.CreateUser(ctx, "race"+strings.Repeat("x", round%5)+string(rune('a'+round)), testHash)
		if err != nil {
			t.Fatal(err)
		}
		digests := make([][32]byte, 8)
		var wg sync.WaitGroup
		for i := range digests {
			_, digests[i], _ = users.NewSessionToken()
			wg.Add(1)
			go func(digest [32]byte) {
				defer wg.Done()
				_, _ = s.CreateSession(ctx, user.ID, digest, time.Hour)
			}(digests[i])
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.SetUserStatus(ctx, user.ID, users.StatusDisabled); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		for _, digest := range digests {
			if _, err := s.GetSession(ctx, digest); !errors.Is(err, users.ErrNotFound) {
				t.Fatalf("round %d: session usable after disable: %v", round, err)
			}
		}
		var stored int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM global_user_sessions s JOIN global_users u ON u.id = s.user_id WHERE u.public_id = $1", user.ID).Scan(&stored); err != nil || stored != 0 {
			t.Fatalf("round %d: %d sessions stored for disabled user", round, stored)
		}
	}
}
