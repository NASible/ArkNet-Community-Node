-- Media-miss events: a user's home Node lacked an active object and playback
-- fell back to Core. Aggregated per (Node, object); deliberately no user or
-- membership column, so neither Core's event table nor the Node learns who.
CREATE TABLE media_miss_events (
    node_id bigint NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    media_object_id bigint NOT NULL REFERENCES media_objects (id) ON DELETE CASCADE,
    miss_count bigint NOT NULL DEFAULT 1 CHECK (miss_count >= 1),
    first_missed_at timestamptz NOT NULL DEFAULT now(),
    last_missed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, media_object_id)
);
