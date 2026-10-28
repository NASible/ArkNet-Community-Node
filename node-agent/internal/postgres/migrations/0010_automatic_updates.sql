-- Operator automatic-update preference. Absent means the default (ON).
ALTER TABLE node_configuration
    ADD CONSTRAINT node_configuration_automatic_updates CHECK (
        NOT settings ? 'automatic_updates'
        OR jsonb_typeof(settings->'automatic_updates') = 'boolean'
    );
