package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// RecordSuccessfulCoreContact persists the database server's wall clock only
// while the local enrollment is still approved.
func (s *Store) RecordSuccessfulCoreContact(ctx context.Context) (time.Time, error) {
	var contact time.Time
	err := s.db.QueryRowContext(ctx, `UPDATE core_enrollment
 SET last_successful_core_contact_at = clock_timestamp()
 WHERE singleton = true AND status = 'APPROVED'
 RETURNING last_successful_core_contact_at`).Scan(&contact)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, errors.New("approved enrollment unavailable")
	}
	if err != nil {
		return time.Time{}, err
	}
	return contact, nil
}
