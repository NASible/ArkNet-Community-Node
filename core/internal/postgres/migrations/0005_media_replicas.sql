-- 0005_media_replicas.sql
-- A replica is one verified catalog object retained by one enrolled node.
-- Local filesystem paths and user identities are deliberately not stored.

CREATE TABLE media_replicas (
    node_id bigint NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    media_object_id bigint NOT NULL REFERENCES media_objects(id) ON DELETE CASCADE,
    available_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, media_object_id)
);