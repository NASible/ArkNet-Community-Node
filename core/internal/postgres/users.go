package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"arknet/core/internal/users"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ users.Repository = (*Store)(nil)

func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (users.User, error) {
	normalized, err := users.NormalizeUsername(username)
	if err != nil || normalized != username {
		return users.User{}, users.ErrInvalidUsername
	}
	id, err := users.NewUserID()
	if err != nil {
		return users.User{}, fmt.Errorf("generate user ID: %w", err)
	}
	var user users.User
	err = s.db.QueryRowContext(ctx,
		"INSERT INTO global_users (public_id, username, password_hash) VALUES ($1, $2, $3) RETURNING public_id, username, status, created_at",
		id, username, passwordHash).Scan(&user.ID, &user.Username, &user.Status, &user.CreatedAt)
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" && pgError.ConstraintName == "global_users_username_key" {
		return users.User{}, users.ErrUsernameTaken
	}
	if err != nil {
		return users.User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

func (s *Store) GetCredentials(ctx context.Context, username string) (users.Credentials, error) {
	var credentials users.Credentials
	err := s.db.QueryRowContext(ctx,
		"SELECT public_id, password_hash, status FROM global_users WHERE username = $1", username).
		Scan(&credentials.UserID, &credentials.PasswordHash, &credentials.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return users.Credentials{}, users.ErrNotFound
	}
	if err != nil {
		return users.Credentials{}, fmt.Errorf("get credentials: %w", err)
	}
	return credentials, nil
}

// SetUserStatus revokes every session in the same transaction when disabling.
func (s *Store) SetUserStatus(ctx context.Context, userID string, status users.Status) (users.User, error) {
	if !users.ValidUserID(userID) {
		return users.User{}, users.ErrInvalidUserID
	}
	if !users.ValidStatus(status) {
		return users.User{}, users.ErrInvalidStatus
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return users.User{}, err
	}
	defer tx.Rollback()
	var user users.User
	var internalID int64
	err = tx.QueryRowContext(ctx,
		`UPDATE global_users SET status = $2, updated_at = CASE WHEN status = $2 THEN updated_at ELSE now() END
		WHERE public_id = $1 RETURNING id, public_id, username, status, created_at`, userID, string(status)).
		Scan(&internalID, &user.ID, &user.Username, &user.Status, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return users.User{}, users.ErrNotFound
	}
	if err != nil {
		return users.User{}, fmt.Errorf("set user status: %w", err)
	}
	if status == users.StatusDisabled {
		if _, err := tx.ExecContext(ctx, "DELETE FROM global_user_sessions WHERE user_id = $1", internalID); err != nil {
			return users.User{}, fmt.Errorf("revoke sessions: %w", err)
		}
	}
	return user, tx.Commit()
}

// CreateSession inserts only for an ACTIVE user, locking the user row so a
// concurrent disable either precedes (no session) or follows (revokes it).
func (s *Store) CreateSession(ctx context.Context, userID string, digest [32]byte, ttl time.Duration) (time.Time, error) {
	if !users.ValidUserID(userID) {
		return time.Time{}, users.ErrInvalidUserID
	}
	if ttl <= 0 {
		return time.Time{}, errors.New("session TTL must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	var internalID int64
	err = tx.QueryRowContext(ctx,
		"SELECT id FROM global_users WHERE public_id = $1 AND status = 'ACTIVE' FOR UPDATE", userID).Scan(&internalID)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, users.ErrInactive
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("lock user: %w", err)
	}
	// Opportunistically discard this user's expired sessions.
	if _, err := tx.ExecContext(ctx, "DELETE FROM global_user_sessions WHERE user_id = $1 AND expires_at <= now()", internalID); err != nil {
		return time.Time{}, fmt.Errorf("prune sessions: %w", err)
	}
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx,
		"INSERT INTO global_user_sessions (token_digest, user_id, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3)) RETURNING expires_at",
		digest[:], internalID, ttl.Seconds()).Scan(&expiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("create session: %w", err)
	}
	return expiresAt, tx.Commit()
}

func (s *Store) GetSession(ctx context.Context, digest [32]byte) (users.Session, error) {
	var session users.Session
	err := s.db.QueryRowContext(ctx,
		`SELECT u.public_id, s.expires_at FROM global_user_sessions s JOIN global_users u ON u.id = s.user_id
		WHERE s.token_digest = $1 AND s.expires_at > now() AND u.status = 'ACTIVE'`, digest[:]).
		Scan(&session.UserID, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return users.Session{}, users.ErrNotFound
	}
	if err != nil {
		return users.Session{}, fmt.Errorf("get session: %w", err)
	}
	return session, nil
}

func (s *Store) DeleteSession(ctx context.Context, digest [32]byte) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM global_user_sessions WHERE token_digest = $1", digest[:]); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}
