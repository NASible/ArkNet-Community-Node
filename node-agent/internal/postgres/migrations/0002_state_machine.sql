ALTER TABLE node_state
    ADD COLUMN changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    ADD CONSTRAINT node_state_valid CHECK (state IN (
        'PROVISIONING', 'ENROLLMENT_PENDING', 'CONNECTED', 'SYNCHRONIZING',
        'MESH', 'DEGRADED', 'OFFLINE', 'RECONNECTING', 'QUARANTINED', 'REVOKED', 'ERROR'
    ));

CREATE TABLE node_state_transitions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    from_state text NOT NULL CHECK (from_state IN (
        'PROVISIONING', 'ENROLLMENT_PENDING', 'CONNECTED', 'SYNCHRONIZING',
        'MESH', 'DEGRADED', 'OFFLINE', 'RECONNECTING', 'QUARANTINED', 'REVOKED', 'ERROR'
    )),
    to_state text NOT NULL CHECK (to_state IN (
        'PROVISIONING', 'ENROLLMENT_PENDING', 'CONNECTED', 'SYNCHRONIZING',
        'MESH', 'DEGRADED', 'OFFLINE', 'RECONNECTING', 'QUARANTINED', 'REVOKED', 'ERROR'
    )),
    transitioned_at timestamptz NOT NULL,
    CHECK (from_state <> to_state)
);
