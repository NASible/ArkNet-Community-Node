ALTER TABLE node_configuration
    ADD CONSTRAINT node_configuration_peer_serving_boolean CHECK (
        NOT settings ? 'peer_serving'
        OR jsonb_typeof(settings->'peer_serving') = 'boolean'
    );
