-- ARKSEC-001 (Prompt 46B, docs/global-login-assertions.md). Global passwords no
-- longer reach the Node. Delete every stored password-derived verifier; ON
-- DELETE CASCADE also revokes every existing global session. Affected members
-- sign in online through Core once. Local users and operators are untouched.
DELETE FROM cached_global_authorizations;

ALTER TABLE cached_global_authorizations DROP COLUMN password_hash;

-- Replace the 24-hour bound with the D3 offline window (30 days).
DO $$
DECLARE c record;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
             WHERE conrelid = 'cached_global_authorizations'::regclass AND contype = 'c'
               AND pg_get_constraintdef(oid) LIKE '%expires_at%'
    LOOP
        EXECUTE format('ALTER TABLE cached_global_authorizations DROP CONSTRAINT %I', c.conname);
    END LOOP;
END $$;

ALTER TABLE cached_global_authorizations RENAME COLUMN validated_at TO issued_at;
ALTER TABLE cached_global_authorizations RENAME COLUMN expires_at TO offline_until;
-- The verified Core-signed assertion is the offline grant (Option A).
ALTER TABLE cached_global_authorizations
    ADD COLUMN assertion text NOT NULL CHECK (char_length(assertion) BETWEEN 3 AND 512),
    ADD CONSTRAINT cached_global_authorizations_offline_window
        CHECK (offline_until > issued_at AND offline_until <= issued_at + interval '30 days');

-- A login started on this Node. challenge is SHA-256 of the browser's
-- HttpOnly login-state cookie; the cookie value itself is never stored.
CREATE TABLE global_login_pending (
    challenge bytea PRIMARY KEY CHECK (octet_length(challenge) = 32),
    nonce bytea NOT NULL UNIQUE CHECK (octet_length(nonce) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '5 minutes')
);

-- Accepted assertion IDs, kept until the assertion can no longer be accepted.
CREATE TABLE global_login_assertions_used (
    assertion_id bytea PRIMARY KEY CHECK (octet_length(assertion_id) = 16),
    expires_at timestamptz NOT NULL
);

-- Per-Node device credentials held by browsers (SHA-256 digests only).
CREATE TABLE global_device_credentials (
    digest bytea PRIMARY KEY CHECK (octet_length(digest) = 32),
    member_id text NOT NULL REFERENCES cached_global_authorizations (member_id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX global_device_credentials_member_id ON global_device_credentials (member_id, created_at);

-- Core's published assertion key set, as last fetched from the configured Core.
CREATE TABLE core_assertion_keys (
    key_id bytea PRIMARY KEY CHECK (octet_length(key_id) = 8),
    public_key bytea NOT NULL UNIQUE CHECK (octet_length(public_key) = 32),
    position smallint NOT NULL UNIQUE CHECK (position BETWEEN 1 AND 9),
    fetched_at timestamptz NOT NULL DEFAULT now()
);
