-- 0004_media_objects.sql
-- Migration 0004: media_objects table.
--
-- Media objects use an opaque, Core-assigned identifier so node filesystem
-- paths never become global identity. The state column constrains to ACTIVE
-- and REMOVED only. InfoHashV2 is a nullable SHA2-256 infohash placeholder
-- for BitTorrent v2 torrent metadata.

CREATE TABLE media_objects (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id text NOT NULL UNIQUE CHECK (public_id ~ '^media_[0-9a-f]{64}$'),
    content_type text NOT NULL,
    title text NOT NULL DEFAULT '',
    size bigint NOT NULL DEFAULT 0,
    state text NOT NULL DEFAULT 'ACTIVE' CHECK (state IN ('ACTIVE', 'REMOVED')),
    info_hash_v2 text CHECK (info_hash_v2 IS NULL OR length(info_hash_v2) = 64),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
