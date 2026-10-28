ALTER TABLE nodes
    ADD COLUMN enrollment_public_key bytea
    CHECK (enrollment_public_key IS NULL OR octet_length(enrollment_public_key) = 32),
    ADD CONSTRAINT nodes_enrollment_public_key_unique UNIQUE (enrollment_public_key);
