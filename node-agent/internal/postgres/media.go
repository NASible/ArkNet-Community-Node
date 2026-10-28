package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/mediaselection"
)

var _ mediaselection.Repository = (*Store)(nil)

func (s *Store) TransitionMediaSelection(ctx context.Context, id string, expected, next mediaselection.State) error {
	result, err := s.db.ExecContext(ctx, `UPDATE media_selections SET local_state=$3,updated_at=clock_timestamp() WHERE id=$1 AND local_state=$2`, id, expected, next)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.New("media state conflict")
	}
	return nil
}

// RetainRemovedMediaSelections consumes explicit Core tombstones only. An
// incomplete catalog page cannot establish that an omitted object was removed.
// Only a verified AVAILABLE copy becomes REMOVED_RETAINED; queued, partial, and
// failed selections become NOT_PRESENT and can no longer be replicated.
func (s *Store) RetainRemovedMediaSelections(ctx context.Context, objects []catalog.Object) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var singleton bool
	if err := tx.QueryRowContext(ctx, "SELECT singleton FROM node_configuration WHERE singleton=true FOR UPDATE").Scan(&singleton); err != nil {
		return err
	}
	for _, object := range objects {
		if object.State != catalog.Removed {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_selections
SET state='REMOVED',
    local_state=CASE WHEN local_state IN ('AVAILABLE','REMOVED_RETAINED') THEN 'REMOVED_RETAINED' ELSE 'NOT_PRESENT' END,
    updated_at=clock_timestamp()
WHERE id=$1 AND state!='REMOVED'`, object.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var selectionIDPattern = regexp.MustCompile(`^media_[0-9a-f]{64}$`)
var selectionHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Match the catalog's validation without making its internal helpers public.
func validSelectionObject(object catalog.Object) bool {
	if !selectionIDPattern.MatchString(object.ID) || object.Size < 0 || object.CreatedAt.IsZero() || object.UpdatedAt.Before(object.CreatedAt) {
		return false
	}
	if object.State != catalog.Active && object.State != catalog.Removed {
		return false
	}
	switch object.ContentType {
	case catalog.Video, catalog.Audio, catalog.Image, catalog.Other:
	default:
		return false
	}
	return object.InfoHashV2 == "" || selectionHashPattern.MatchString(object.InfoHashV2)
}

func (s *Store) ListMediaSelections(ctx context.Context) ([]mediaselection.Object, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,title,size,state,local_state FROM media_selections ORDER BY created_at,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := make([]mediaselection.Object, 0)
	for rows.Next() {
		var object mediaselection.Object
		if err := rows.Scan(&object.ID, &object.Title, &object.Size, &object.State, &object.LocalState); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

func (s *Store) QueueMediaSelection(ctx context.Context, object catalog.Object, allocation int64) (mediaselection.Object, error) {
	// Callers supply the object freshly fetched from Core, never a client size.
	if object.State == catalog.Removed {
		return mediaselection.Object{}, mediaselection.ErrRemoved
	}
	if !validSelectionObject(object) {
		return mediaselection.Object{}, mediaselection.ErrInvalidObject
	}
	if allocation < -1 {
		return mediaselection.Object{}, mediaselection.ErrAllocationExceeded
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mediaselection.Object{}, err
	}
	defer tx.Rollback()
	// All queue attempts serialize on the configuration singleton. The aggregate
	// below must run as a separate statement after acquiring this row lock.
	var singleton bool
	if err := tx.QueryRowContext(ctx, "SELECT singleton FROM node_configuration WHERE singleton=true FOR UPDATE").Scan(&singleton); err != nil {
		return mediaselection.Object{}, err
	}
	var selected mediaselection.Object
	err = tx.QueryRowContext(ctx, "SELECT id,title,size,state,local_state FROM media_selections WHERE id=$1", object.ID).Scan(&selected.ID, &selected.Title, &selected.Size, &selected.State, &selected.LocalState)
	if err == nil {
		if selected.State == catalog.Removed || selected.LocalState == mediaselection.RemovedRetained {
			return mediaselection.Object{}, mediaselection.ErrRemoved
		}
		return selected, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return mediaselection.Object{}, err
	}
	var fits bool
	// SUM(bigint) is numeric in PostgreSQL: do not scan or add it in int64.
	if err := tx.QueryRowContext(ctx, `SELECT $1::numeric = -1 OR ($1::numeric > 0 AND COALESCE(SUM(size) FILTER (WHERE local_state!='NOT_PRESENT'),0) + $2::numeric <= $1::numeric) FROM media_selections`, allocation, object.Size).Scan(&fits); err != nil {
		return mediaselection.Object{}, err
	}
	if !fits {
		return mediaselection.Object{}, mediaselection.ErrAllocationExceeded
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO media_selections (id,title,size,state,local_state) VALUES ($1,$2,$3,$4,'QUEUED') RETURNING id,title,size,state,local_state`, object.ID, object.Title, object.Size, object.State).Scan(&selected.ID, &selected.Title, &selected.Size, &selected.State, &selected.LocalState)
	if err != nil {
		return mediaselection.Object{}, err
	}
	return selected, tx.Commit()
}
