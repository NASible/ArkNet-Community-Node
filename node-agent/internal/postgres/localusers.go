package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"arknet/node-agent/internal/localusers"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ localusers.Repository = (*Store)(nil)

func (s *Store) CreateLocalUser(ctx context.Context, username, passwordHash string) (localusers.User, error) {
	normalized, err := localusers.NormalizeUsername(username)
	if err != nil || normalized != username {
		return localusers.User{}, localusers.ErrInvalidUsername
	}
	id, err := localusers.NewUserID()
	if err != nil {
		return localusers.User{}, fmt.Errorf("generate local user ID: %w", err)
	}
	var user localusers.User
	err = s.db.QueryRowContext(ctx,
		"INSERT INTO local_users (public_id, username, password_hash) VALUES ($1, $2, $3) RETURNING public_id, username, status, created_at",
		id, username, passwordHash).Scan(&user.ID, &user.Username, &user.Status, &user.CreatedAt)
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" && pgError.ConstraintName == "local_users_username_key" {
		return localusers.User{}, localusers.ErrUsernameTaken
	}
	if err != nil {
		return localusers.User{}, fmt.Errorf("create local user: %w", err)
	}
	return user, nil
}

// ListLocalUsers returns up to limit+1 users ordered by username.
func (s *Store) ListLocalUsers(ctx context.Context, limit int) ([]localusers.User, error) {
	if limit < 1 || limit > localusers.MaxListedUsers {
		return nil, errors.New("invalid local user page")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT public_id, username, status, created_at FROM local_users ORDER BY username LIMIT $1", limit+1)
	if err != nil {
		return nil, fmt.Errorf("list local users: %w", err)
	}
	defer rows.Close()
	users := []localusers.User{}
	for rows.Next() {
		var user localusers.User
		if err := rows.Scan(&user.ID, &user.Username, &user.Status, &user.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan local user: %w", err)
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Store) LocalCredentials(ctx context.Context, username string) (localusers.Credentials, error) {
	var credentials localusers.Credentials
	err := s.db.QueryRowContext(ctx, "SELECT public_id, password_hash, status FROM local_users WHERE username = $1", username).
		Scan(&credentials.UserID, &credentials.PasswordHash, &credentials.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return localusers.Credentials{}, localusers.ErrNotFound
	}
	if err != nil {
		return localusers.Credentials{}, fmt.Errorf("get local credentials: %w", err)
	}
	return credentials, nil
}

func (s *Store) SetLocalUserStatus(ctx context.Context, userID string, status localusers.Status) (localusers.User, error) {
	if !localusers.ValidUserID(userID) {
		return localusers.User{}, localusers.ErrInvalidUserID
	}
	if !localusers.ValidStatus(status) {
		return localusers.User{}, localusers.ErrInvalidStatus
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return localusers.User{}, err
	}
	defer tx.Rollback()
	var user localusers.User
	var internalID int64
	err = tx.QueryRowContext(ctx,
		`UPDATE local_users SET status = $2, updated_at = CASE WHEN status = $2 THEN updated_at ELSE now() END
		WHERE public_id = $1 RETURNING id, public_id, username, status, created_at`, userID, string(status)).
		Scan(&internalID, &user.ID, &user.Username, &user.Status, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return localusers.User{}, localusers.ErrNotFound
	}
	if err != nil {
		return localusers.User{}, fmt.Errorf("set local user status: %w", err)
	}
	if status == localusers.StatusDisabled {
		if _, err := tx.ExecContext(ctx, "DELETE FROM local_user_sessions WHERE user_id = $1", internalID); err != nil {
			return localusers.User{}, fmt.Errorf("revoke local sessions: %w", err)
		}
	}
	return user, tx.Commit()
}

func (s *Store) CreateLocalSession(ctx context.Context, userID string, digest [32]byte, ttl time.Duration) (time.Time, error) {
	if !localusers.ValidUserID(userID) {
		return time.Time{}, localusers.ErrInvalidUserID
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
	err = tx.QueryRowContext(ctx, "SELECT id FROM local_users WHERE public_id = $1 AND status = 'ACTIVE' FOR UPDATE", userID).Scan(&internalID)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, localusers.ErrInactive
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("lock local user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM local_user_sessions WHERE user_id = $1 AND expires_at <= now()", internalID); err != nil {
		return time.Time{}, fmt.Errorf("prune local sessions: %w", err)
	}
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx,
		"INSERT INTO local_user_sessions (token_digest, user_id, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3)) RETURNING expires_at",
		digest[:], internalID, ttl.Seconds()).Scan(&expiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("create local session: %w", err)
	}
	return expiresAt, tx.Commit()
}

func (s *Store) GetLocalSession(ctx context.Context, digest [32]byte) (localusers.Session, error) {
	var session localusers.Session
	err := s.db.QueryRowContext(ctx,
		`SELECT u.public_id, s.expires_at FROM local_user_sessions s JOIN local_users u ON u.id = s.user_id
		WHERE s.token_digest = $1 AND s.expires_at > now() AND u.status = 'ACTIVE'`, digest[:]).
		Scan(&session.UserID, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return localusers.Session{}, localusers.ErrNotFound
	}
	if err != nil {
		return localusers.Session{}, fmt.Errorf("get local session: %w", err)
	}
	return session, nil
}

func (s *Store) DeleteLocalSession(ctx context.Context, digest [32]byte) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM local_user_sessions WHERE token_digest = $1", digest[:]); err != nil {
		return fmt.Errorf("delete local session: %w", err)
	}
	return nil
}
