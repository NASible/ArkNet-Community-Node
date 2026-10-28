ALTER TABLE nodes
    ADD COLUMN current_state text CHECK (current_state IN (
        'PROVISIONING', 'ENROLLMENT_PENDING', 'CONNECTED', 'SYNCHRONIZING',
        'MESH', 'DEGRADED', 'OFFLINE', 'RECONNECTING', 'QUARANTINED', 'REVOKED', 'ERROR'
    )),
    ADD COLUMN reported_health jsonb CHECK (
        reported_health IS NULL OR (
            jsonb_typeof(reported_health) = 'object'
            AND reported_health ? 'healthy'
            AND reported_health ? 'database_healthy'
            AND jsonb_typeof(reported_health->'healthy') = 'boolean'
            AND jsonb_typeof(reported_health->'database_healthy') = 'boolean'
            AND reported_health - 'healthy' - 'database_healthy' = '{}'::jsonb
        )
    ),
    ADD COLUMN heartbeat_proof_at bigint;
