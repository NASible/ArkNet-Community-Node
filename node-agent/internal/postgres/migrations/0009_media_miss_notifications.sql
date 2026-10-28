-- Node-local media-miss notifications synchronized from Core. No user or
-- member identity exists in Core's miss report, so none is stored here.
CREATE TABLE media_miss_notifications (
    media_id text PRIMARY KEY CHECK (media_id ~ '^media_[0-9a-f]{64}$'),
    title text NOT NULL,
    size bigint NOT NULL CHECK (size >= 0),
    miss_count bigint NOT NULL CHECK (miss_count >= 1),
    state text NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING', 'IGNORED')),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

ALTER TABLE node_configuration
    ADD CONSTRAINT node_configuration_media_miss_policy CHECK (
        NOT settings ? 'media_miss_policy'
        OR settings->>'media_miss_policy' IN ('MANUAL', 'ASK', 'AUTOMATIC')
    );
