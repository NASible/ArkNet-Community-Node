-- Prompt 48B: revocations Core must push to operator relays.
-- A row is written in the same transaction that makes a Node BLOCKED.
CREATE TABLE relay_revocation_events (
    node_id bigint PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    decided_at timestamptz NOT NULL DEFAULT now()
);

-- One row per relay that has been attempted. A delivery is pending while
-- acknowledged_at is NULL or no row exists for a configured relay.
CREATE TABLE relay_revocation_deliveries (
    node_id bigint NOT NULL REFERENCES relay_revocation_events(node_id) ON DELETE CASCADE,
    relay_id text NOT NULL CHECK (relay_id ~ '^[a-z0-9][a-z0-9.-]{0,63}$'),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text CHECK (last_error IS NULL OR length(last_error) BETWEEN 1 AND 64),
    last_attempt_at timestamptz,
    acknowledged_at timestamptz,
    PRIMARY KEY (node_id, relay_id)
);
