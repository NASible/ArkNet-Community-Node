CREATE TABLE media_selections (
    id text PRIMARY KEY CHECK (id ~ '^media_[0-9a-f]{64}$'),
    title text NOT NULL,
    size bigint NOT NULL CHECK (size >= 0),
    state text NOT NULL CHECK (state IN ('ACTIVE', 'REMOVED')),
    local_state text NOT NULL CHECK (local_state IN ('NOT_PRESENT', 'QUEUED', 'DOWNLOADING', 'AVAILABLE', 'REMOVED_RETAINED', 'ERROR')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
