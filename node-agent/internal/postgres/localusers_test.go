package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"arknet/node-agent/internal/localusers"
)

const localTestHash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g"

func initializedStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	if err := s.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLocalUsersStoreOnlyHashesAndRejectsDuplicatesOnThisNode(t *testing.T) {
	s := initializedStore(t)
	ctx := context.Background()
	user, err := s.CreateLocalUser(ctx, "alice", localTestHash)
	if err != nil || !localusers.ValidUserID(user.ID) || user.Status != localusers.StatusActive {
		t.Fatalf("%+v %v", user, err)
	}
	if _, err := s.CreateLocalUser(ctx, "alice", localTestHash); !errors.Is(err, localusers.ErrUsernameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.CreateLocalUser(ctx, "bob", "plaintext-password"); err == nil {
		t.Fatal("schema accepted a plaintext password")
	}
	for _, statement := range []string{
		"INSERT INTO local_users (public_id, username, password_hash) VALUES ('user_" + user.ID[6:] + "', 'carol', '" + localTestHash + "')",
		"UPDATE local_users SET status = 'GLOBAL'",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted %q", statement)
		}
	}
	users, err := s.ListLocalUsers(ctx, 10)
	if err != nil || len(users) != 1 || users[0].ID != user.ID || users[0].Username != "alice" {
		t.Fatalf("%+v %v", users, err)
	}
}

func TestSameUsernameOnAnotherNodeIsADifferentAccount(t *testing.T) {
	nodeA, nodeB := initializedStore(t), initializedStore(t) // separate Node databases
	ctx := context.Background()
	hashA, _ := localusers.HashPassword(ctx, "node A password only")
	hashB, _ := localusers.HashPassword(ctx, "node B password only")
	onA, err := nodeA.CreateLocalUser(ctx, "alice", hashA)
	if err != nil {
		t.Fatal(err)
	}
	onB, err := nodeB.CreateLocalUser(ctx, "alice", hashB)
	if err != nil {
		t.Fatal(err)
	}
	if onA.ID == onB.ID {
		t.Fatal("same username produced the same account ID on two Nodes")
	}
	credentialsB, err := nodeB.LocalCredentials(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := localusers.VerifyPassword(ctx, credentialsB.PasswordHash, "node A password only"); ok || credentialsB.UserID == onA.ID {
		t.Fatal("Node A's credentials authenticate on Node B")
	}
	_, digest, _ := localusers.NewSessionToken()
	if _, err := nodeA.CreateLocalSession(ctx, onA.ID, digest, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeB.GetLocalSession(ctx, digest); !errors.Is(err, localusers.ErrNotFound) {
		t.Fatal("Node A's session is valid on Node B")
	}
	if _, err := nodeB.CreateLocalSession(ctx, onA.ID, digest, time.Hour); !errors.Is(err, localusers.ErrInactive) {
		t.Fatal("Node B created a session for Node A's account ID")
	}
}

func TestLocalSessionsExpireAndDisableRevokes(t *testing.T) {
	s := initializedStore(t)
	ctx := context.Background()
	user, _ := s.CreateLocalUser(ctx, "alice", localTestHash)
	_, digest, _ := localusers.NewSessionToken()
	expiresAt, err := s.CreateLocalSession(ctx, user.ID, digest, localusers.SessionTTL)
	if err != nil || time.Until(expiresAt) > 13*time.Hour || time.Until(expiresAt) < 11*time.Hour {
		t.Fatalf("%v %v", expiresAt, err)
	}
	if session, err := s.GetLocalSession(ctx, digest); err != nil || session.UserID != user.ID {
		t.Fatalf("%+v %v", session, err)
	}
	_, expired, _ := localusers.NewSessionToken()
	if _, err := s.CreateLocalSession(ctx, user.ID, expired, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE local_user_sessions SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE token_digest = $1", expired[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLocalSession(ctx, expired); !errors.Is(err, localusers.ErrNotFound) {
		t.Fatal("expired session accepted")
	}
	if _, err := s.SetLocalUserStatus(ctx, user.ID, localusers.StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLocalSession(ctx, digest); !errors.Is(err, localusers.ErrNotFound) {
		t.Fatal("disabled user's session survived")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM local_user_sessions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("sessions not revoked: %d", count)
	}
	_, fresh, _ := localusers.NewSessionToken()
	if _, err := s.CreateLocalSession(ctx, user.ID, fresh, time.Hour); !errors.Is(err, localusers.ErrInactive) {
		t.Fatal("disabled user got a session")
	}
	if _, err := s.SetLocalUserStatus(ctx, "local_"+user.ID[6:68]+"00", localusers.StatusActive); !errors.Is(err, localusers.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}
