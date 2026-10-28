package postgres

import (
	"arknet/core/internal/media"
	"arknet/core/internal/store"
	"context"
	"database/sql"
	"fmt"
)

var _ store.MediaCatalogRepository = (*Store)(nil)

func (s *Store) CreateMediaObject(ctx context.Context, object store.Object) (store.Object, error) {
	if object.ID != "" {
		return store.Object{}, media.ErrInvalidMediaID
	}
	if object.State == "" {
		object.State = media.ObjectActive
	}
	if err := media.Validate(object); err != nil {
		return store.Object{}, err
	}
	id, err := media.GenerateMediaID()
	if err != nil {
		return store.Object{}, fmt.Errorf("generate media ID: %w", err)
	}
	object.ID = id
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO media_objects (public_id, content_type, title, size, state, info_hash_v2)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		RETURNING public_id, content_type, title, size, state, COALESCE(info_hash_v2, ''), created_at, updated_at`,
		object.ID, string(object.ObjectType), object.Title, object.Size, string(object.State), object.InfoHashV2).
		Scan(&object.ID, &object.ObjectType, &object.Title, &object.Size, &object.State, &object.InfoHashV2, &object.CreatedAt, &object.UpdatedAt)
	if err != nil {
		return store.Object{}, fmt.Errorf("create media object: %w", err)
	}
	return object, nil
}

func (s *Store) GetMediaObjectByPublicID(ctx context.Context, id string) (store.Object, error) {
	if !media.ValidMediaID(id) {
		return store.Object{}, media.ErrInvalidMediaID
	}
	var o store.Object
	err := s.db.QueryRowContext(ctx,
		"SELECT public_id, content_type, title, size, state, COALESCE(info_hash_v2, ''), created_at, updated_at FROM media_objects WHERE public_id = $1", id).
		Scan(&o.ID, &o.ObjectType, &o.Title, &o.Size, &o.State, &o.InfoHashV2, &o.CreatedAt, &o.UpdatedAt)
	if err == sql.ErrNoRows {
		return store.Object{}, store.ErrNotFound
	}
	if err != nil {
		return store.Object{}, fmt.Errorf("get media object: %w", err)
	}
	return o, nil
}

func (s *Store) ListMediaObjects(ctx context.Context, includeRemoved bool, limit int) ([]store.Object, error) {
	if limit < 1 || limit > 100 {
		return nil, media.ErrInvalidLimit
	}
	query := "SELECT public_id, content_type, title, size, state, COALESCE(info_hash_v2, ''), created_at, updated_at FROM media_objects"
	if !includeRemoved {
		query += " WHERE state = 'ACTIVE'"
	}
	query += " ORDER BY created_at DESC LIMIT $1"
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("list media objects: %w", err)
	}
	defer rows.Close()
	var objects []store.Object
	for rows.Next() {
		var o store.Object
		if err := rows.Scan(&o.ID, &o.ObjectType, &o.Title, &o.Size, &o.State, &o.InfoHashV2, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan media object: %w", err)
		}
		objects = append(objects, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list media objects: %w", err)
	}
	return objects, nil
}

func (s *Store) UpdateMediaObjectState(ctx context.Context, id string, state store.ObjectState) (store.Object, error) {
	if !media.ValidMediaID(id) {
		return store.Object{}, media.ErrInvalidMediaID
	}
	if state != media.ObjectRemoved {
		return store.Object{}, media.ErrInvalidState
	}
	now := media.ClockNow()
	result, err := s.db.ExecContext(ctx,
		"UPDATE media_objects SET state = $2, updated_at = $3 WHERE public_id = $1 AND state = 'ACTIVE'",
		id, string(state), now)
	if err != nil {
		return store.Object{}, fmt.Errorf("update media state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return store.Object{}, fmt.Errorf("update media state: %w", err)
	}
	if affected == 0 {
		existing, lookupErr := s.GetMediaObjectByPublicID(ctx, id)
		if lookupErr != nil {
			return store.Object{}, lookupErr
		}
		if existing.State == media.ObjectRemoved {
			return store.Object{}, media.ErrInvalidState
		}
		return store.Object{}, fmt.Errorf("update media state: no row updated")
	}
	return s.GetMediaObjectByPublicID(ctx, id)
}
