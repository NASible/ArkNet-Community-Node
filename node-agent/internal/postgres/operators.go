package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"arknet/node-agent/internal/operators"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ operators.Repository = (*Store)(nil)

func operatorEvent(ctx context.Context, tx *sql.Tx, operatorID *int64, event string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO operator_events (operator_id, event) VALUES ($1, $2)", operatorID, event)
	return err
}

func (s *Store) CountActiveOperators(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM operators WHERE status = 'ACTIVE'").Scan(&count)
	return count, err
}

// IssueSetupCode replaces any unused SETUP code. Without additional it is
// refused while an ACTIVE operator exists.
func (s *Store) IssueSetupCode(ctx context.Context, digest [32]byte, additional bool) (time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	// Serialize issuing against concurrent setup so the "no operator" check holds.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('arknet-operator-setup'))"); err != nil {
		return time.Time{}, err
	}
	if !additional {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM operators WHERE status = 'ACTIVE')").Scan(&exists); err != nil {
			return time.Time{}, err
		}
		if exists {
			return time.Time{}, operators.ErrOperatorsExist
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM operator_setup_codes WHERE purpose = 'SETUP' AND used_at IS NULL"); err != nil {
		return time.Time{}, err
	}
	var expiresAt time.Time
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO operator_setup_codes (code_digest, purpose, expires_at) VALUES ($1, 'SETUP', now() + make_interval(secs => $2)) RETURNING expires_at",
		digest[:], operators.SetupCodeTTL.Seconds()).Scan(&expiresAt); err != nil {
		return time.Time{}, fmt.Errorf("issue setup code: %w", err)
	}
	if err := operatorEvent(ctx, tx, nil, "SETUP_CODE_ISSUED"); err != nil {
		return time.Time{}, err
	}
	return expiresAt, tx.Commit()
}

// IssueRecoveryCode revokes the operator's sessions and pending enrollments
// immediately and replaces any earlier unused recovery code for it.
func (s *Store) IssueRecoveryCode(ctx context.Context, username string, digest [32]byte) (time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM operators WHERE username = $1 FOR UPDATE", username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, operators.ErrNotFound
	}
	if err != nil {
		return time.Time{}, err
	}
	for _, statement := range []string{
		"DELETE FROM operator_sessions WHERE operator_id = $1",
		"DELETE FROM operator_enrollments WHERE operator_id = $1",
		"DELETE FROM operator_setup_codes WHERE operator_id = $1 AND used_at IS NULL",
	} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return time.Time{}, err
		}
	}
	var expiresAt time.Time
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO operator_setup_codes (code_digest, purpose, operator_id, expires_at) VALUES ($1, 'RECOVERY', $2, now() + make_interval(secs => $3)) RETURNING expires_at",
		digest[:], id, operators.SetupCodeTTL.Seconds()).Scan(&expiresAt); err != nil {
		return time.Time{}, fmt.Errorf("issue recovery code: %w", err)
	}
	if err := operatorEvent(ctx, tx, &id, "RECOVERY_CODE_ISSUED"); err != nil {
		return time.Time{}, err
	}
	return expiresAt, tx.Commit()
}

// BeginEnrollment consumes a code exactly once. A RECOVERY code only works for
// the username it was issued for. A SETUP code is not consumed if the username
// is taken.
func (s *Store) BeginEnrollment(ctx context.Context, codeDigest [32]byte, username, passwordHash string, totpSecret []byte, enrollmentDigest [32]byte) (operators.Enrollment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return operators.Enrollment{}, err
	}
	defer tx.Rollback()
	var purpose operators.Purpose
	var operatorID sql.NullInt64
	var boundUsername sql.NullString
	err = tx.QueryRowContext(ctx, `UPDATE operator_setup_codes c SET used_at = now()
		WHERE c.code_digest = $1 AND c.used_at IS NULL AND c.expires_at > now()
		RETURNING c.purpose, c.operator_id, (SELECT username FROM operators o WHERE o.id = c.operator_id)`, codeDigest[:]).
		Scan(&purpose, &operatorID, &boundUsername)
	if errors.Is(err, sql.ErrNoRows) {
		return operators.Enrollment{}, operators.ErrInvalidCode
	}
	if err != nil {
		return operators.Enrollment{}, fmt.Errorf("consume code: %w", err)
	}
	if purpose == operators.PurposeRecovery && boundUsername.String != username {
		return operators.Enrollment{}, operators.ErrInvalidCode // rolls back: the code stays usable
	}
	if purpose == operators.PurposeSetup {
		var taken bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM operators WHERE username = $1)", username).Scan(&taken); err != nil {
			return operators.Enrollment{}, err
		}
		if taken {
			return operators.Enrollment{}, operators.ErrUsernameTaken
		}
	}
	enrollment := operators.Enrollment{Purpose: purpose, Username: username, TOTPSecret: totpSecret}
	if err := tx.QueryRowContext(ctx, `INSERT INTO operator_enrollments
		(token_digest, purpose, operator_id, username, password_hash, totp_secret, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7)) RETURNING expires_at`,
		enrollmentDigest[:], string(purpose), operatorID, username, passwordHash, totpSecret, operators.EnrollmentTTL.Seconds()).
		Scan(&enrollment.ExpiresAt); err != nil {
		return operators.Enrollment{}, fmt.Errorf("start enrollment: %w", err)
	}
	var eventOperator *int64
	if operatorID.Valid {
		eventOperator = &operatorID.Int64
	}
	if err := operatorEvent(ctx, tx, eventOperator, "ENROLLMENT_STARTED"); err != nil {
		return operators.Enrollment{}, err
	}
	return enrollment, tx.Commit()
}

func (s *Store) PendingEnrollment(ctx context.Context, enrollmentDigest [32]byte) (operators.Enrollment, error) {
	var enrollment operators.Enrollment
	err := s.db.QueryRowContext(ctx,
		"SELECT purpose, username, totp_secret, expires_at FROM operator_enrollments WHERE token_digest = $1 AND expires_at > now()",
		enrollmentDigest[:]).Scan(&enrollment.Purpose, &enrollment.Username, &enrollment.TOTPSecret, &enrollment.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operators.Enrollment{}, operators.ErrInvalidCode
	}
	return enrollment, err
}

// ConfirmEnrollment consumes the enrollment, creates or re-credentials the
// operator, stores recovery-code digests and starts a session, atomically.
func (s *Store) ConfirmEnrollment(ctx context.Context, enrollmentDigest [32]byte, totpStep int64, recoveryDigests [][32]byte, sessionDigest [32]byte) (operators.Session, error) {
	if totpStep <= 0 || len(recoveryDigests) != operators.RecoveryCodeCount {
		return operators.Session{}, errors.New("invalid enrollment confirmation")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return operators.Session{}, err
	}
	defer tx.Rollback()
	var purpose operators.Purpose
	var operatorID sql.NullInt64
	var username, passwordHash string
	var secret []byte
	err = tx.QueryRowContext(ctx, `DELETE FROM operator_enrollments WHERE token_digest = $1 AND expires_at > now()
		RETURNING purpose, operator_id, username, password_hash, totp_secret`, enrollmentDigest[:]).
		Scan(&purpose, &operatorID, &username, &passwordHash, &secret)
	if errors.Is(err, sql.ErrNoRows) {
		return operators.Session{}, operators.ErrInvalidCode
	}
	if err != nil {
		return operators.Session{}, fmt.Errorf("consume enrollment: %w", err)
	}
	var id int64
	event := "OPERATOR_CREATED"
	if purpose == operators.PurposeSetup {
		publicID, err := operators.NewOperatorID()
		if err != nil {
			return operators.Session{}, err
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO operators (public_id, username, password_hash, totp_secret, totp_last_step)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, publicID, username, passwordHash, secret, totpStep).Scan(&id)
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "23505" && pgError.ConstraintName == "operators_username_key" {
			return operators.Session{}, operators.ErrUsernameTaken
		}
		if err != nil {
			return operators.Session{}, fmt.Errorf("create operator: %w", err)
		}
	} else {
		// Shell recovery is a local-root decision, so it also re-enables a disabled account.
		id, event = operatorID.Int64, "OPERATOR_RECOVERED"
		for _, statement := range []string{
			"DELETE FROM operator_sessions WHERE operator_id = $1",
			"DELETE FROM operator_recovery_codes WHERE operator_id = $1",
		} {
			if _, err := tx.ExecContext(ctx, statement, id); err != nil {
				return operators.Session{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE operators SET password_hash = $2, totp_secret = $3, totp_last_step = $4,
			status = 'ACTIVE', failed_logins = 0, retry_after = NULL, credentials_changed_at = now(), updated_at = now()
			WHERE id = $1`, id, passwordHash, secret, totpStep); err != nil {
			return operators.Session{}, fmt.Errorf("recover operator: %w", err)
		}
	}
	for _, digest := range recoveryDigests {
		if _, err := tx.ExecContext(ctx, "INSERT INTO operator_recovery_codes (operator_id, code_digest) VALUES ($1, $2)", id, digest[:]); err != nil {
			return operators.Session{}, fmt.Errorf("store recovery codes: %w", err)
		}
	}
	if err := operatorEvent(ctx, tx, &id, event); err != nil {
		return operators.Session{}, err
	}
	session, err := insertOperatorSession(ctx, tx, id, sessionDigest)
	if err != nil {
		return operators.Session{}, err
	}
	return session, tx.Commit()
}

// insertOperatorSession enforces the per-operator session cap (oldest first),
// records the sign-in and returns the new session.
func insertOperatorSession(ctx context.Context, tx *sql.Tx, id int64, digest [32]byte) (operators.Session, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM operator_sessions WHERE operator_id = $1 AND (expires_at <= now() OR idle_expires_at <= now()
		OR token_digest IN (SELECT token_digest FROM operator_sessions WHERE operator_id = $1
		ORDER BY created_at DESC OFFSET $2))`, id, operators.MaxSessionsPerOperator-1); err != nil {
		return operators.Session{}, fmt.Errorf("prune sessions: %w", err)
	}
	var session operators.Session
	err := tx.QueryRowContext(ctx, `INSERT INTO operator_sessions (token_digest, operator_id, idle_expires_at, expires_at)
		VALUES ($1, $2, now() + make_interval(secs => $3), now() + make_interval(secs => $4))
		RETURNING (SELECT public_id FROM operators WHERE id = $2), (SELECT username FROM operators WHERE id = $2),
		authenticated_at, idle_expires_at, expires_at`,
		digest[:], id, operators.SessionIdleTTL.Seconds(), operators.SessionTTL.Seconds()).
		Scan(&session.OperatorID, &session.Username, &session.AuthenticatedAt, &session.IdleExpiresAt, &session.ExpiresAt)
	if err != nil {
		return operators.Session{}, fmt.Errorf("create session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE operators SET last_sign_in_at = now(), failed_logins = 0, retry_after = NULL WHERE id = $1", id); err != nil {
		return operators.Session{}, err
	}
	return session, operatorEvent(ctx, tx, &id, "SIGNED_IN")
}

func (s *Store) OperatorCredentials(ctx context.Context, username string) (operators.Credentials, error) {
	var c operators.Credentials
	var retryAfter sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT public_id, username, password_hash, totp_secret, totp_last_step, status,
		CASE WHEN retry_after > now() THEN retry_after END FROM operators WHERE username = $1`, username).
		Scan(&c.OperatorID, &c.Username, &c.PasswordHash, &c.TOTPSecret, &c.TOTPLastStep, &c.Status, &retryAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return operators.Credentials{}, operators.ErrNotFound
	}
	if err != nil {
		return operators.Credentials{}, fmt.Errorf("operator credentials: %w", err)
	}
	if retryAfter.Valid {
		c.RetryAfter = &retryAfter.Time
	}
	return c, nil
}

func (s *Store) RecordFailedLogin(ctx context.Context, operatorID string) error {
	if !operators.ValidOperatorID(operatorID) {
		return operators.ErrInvalidID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	var failures int
	err = tx.QueryRowContext(ctx, "UPDATE operators SET failed_logins = failed_logins + 1 WHERE public_id = $1 RETURNING id, failed_logins", operatorID).
		Scan(&id, &failures)
	if errors.Is(err, sql.ErrNoRows) {
		return operators.ErrNotFound
	}
	if err != nil {
		return err
	}
	if delay := operators.FailureDelay(failures); delay > 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE operators SET retry_after = now() + make_interval(secs => $2) WHERE id = $1", id, delay.Seconds()); err != nil {
			return err
		}
	}
	if err := operatorEvent(ctx, tx, &id, "SIGN_IN_FAILED"); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateOperatorSession re-checks status and atomically spends the second
// factor: a TOTP step must be newer than the last accepted one, and a recovery
// code must be unused.
func (s *Store) CreateOperatorSession(ctx context.Context, operatorID string, factor operators.SecondFactor, sessionDigest [32]byte) (operators.Session, error) {
	if !operators.ValidOperatorID(operatorID) {
		return operators.Session{}, operators.ErrInvalidID
	}
	if (factor.TOTPStep > 0) == (factor.RecoveryDigest != nil) {
		return operators.Session{}, errors.New("exactly one second factor is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return operators.Session{}, err
	}
	defer tx.Rollback()
	var id int64
	var status operators.Status
	var lastStep int64
	err = tx.QueryRowContext(ctx, "SELECT id, status, totp_last_step FROM operators WHERE public_id = $1 FOR UPDATE", operatorID).
		Scan(&id, &status, &lastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return operators.Session{}, operators.ErrNotFound
	}
	if err != nil {
		return operators.Session{}, err
	}
	if status != operators.StatusActive {
		return operators.Session{}, operators.ErrInactive
	}
	if factor.TOTPStep > 0 {
		if factor.TOTPStep <= lastStep {
			return operators.Session{}, operators.ErrReplay
		}
		if _, err := tx.ExecContext(ctx, "UPDATE operators SET totp_last_step = $2 WHERE id = $1", id, factor.TOTPStep); err != nil {
			return operators.Session{}, err
		}
	} else {
		result, err := tx.ExecContext(ctx, "UPDATE operator_recovery_codes SET used_at = now() WHERE operator_id = $1 AND code_digest = $2 AND used_at IS NULL",
			id, factor.RecoveryDigest[:])
		if err != nil {
			return operators.Session{}, err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return operators.Session{}, operators.ErrReplay
		}
		if err := operatorEvent(ctx, tx, &id, "RECOVERY_CODE_USED"); err != nil {
			return operators.Session{}, err
		}
	}
	session, err := insertOperatorSession(ctx, tx, id, sessionDigest)
	if err != nil {
		return operators.Session{}, err
	}
	return session, tx.Commit()
}

func (s *Store) GetOperatorSession(ctx context.Context, digest [32]byte) (operators.Session, error) {
	var session operators.Session
	err := s.db.QueryRowContext(ctx, `UPDATE operator_sessions s
		SET last_seen_at = now(), idle_expires_at = LEAST(now() + make_interval(secs => $2), s.expires_at)
		FROM operators o
		WHERE s.token_digest = $1 AND o.id = s.operator_id AND o.status = 'ACTIVE'
		  AND s.expires_at > now() AND s.idle_expires_at > now()
		RETURNING o.public_id, o.username, s.authenticated_at, s.idle_expires_at, s.expires_at`,
		digest[:], operators.SessionIdleTTL.Seconds()).
		Scan(&session.OperatorID, &session.Username, &session.AuthenticatedAt, &session.IdleExpiresAt, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operators.Session{}, operators.ErrNotFound
	}
	if err != nil {
		return operators.Session{}, fmt.Errorf("get operator session: %w", err)
	}
	return session, nil
}

func (s *Store) DeleteOperatorSession(ctx context.Context, digest [32]byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, "DELETE FROM operator_sessions WHERE token_digest = $1 RETURNING operator_id", digest[:]).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // idempotent
	}
	if err != nil {
		return err
	}
	if err := operatorEvent(ctx, tx, &id, "SIGNED_OUT"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListOperators(ctx context.Context) ([]operators.Operator, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT public_id, username, status, created_at, last_sign_in_at FROM operators ORDER BY username LIMIT $1",
		operators.MaxListedOperators)
	if err != nil {
		return nil, fmt.Errorf("list operators: %w", err)
	}
	defer rows.Close()
	list := []operators.Operator{}
	for rows.Next() {
		var op operators.Operator
		var last sql.NullTime
		if err := rows.Scan(&op.ID, &op.Username, &op.Status, &op.CreatedAt, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			op.LastSignInAt = &last.Time
		}
		list = append(list, op)
	}
	return list, rows.Err()
}

func (s *Store) DisableOperator(ctx context.Context, username string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, "UPDATE operators SET status = 'DISABLED', updated_at = now() WHERE username = $1 RETURNING id", username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return operators.ErrNotFound
	}
	if err != nil {
		return err
	}
	for _, statement := range []string{
		"DELETE FROM operator_sessions WHERE operator_id = $1",
		"DELETE FROM operator_enrollments WHERE operator_id = $1",
	} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return err
		}
	}
	if err := operatorEvent(ctx, tx, &id, "OPERATOR_DISABLED"); err != nil {
		return err
	}
	return tx.Commit()
}
