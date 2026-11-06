-- Prompt 49 (ARKSEC-005): administrator identities, scoped API tokens and an
-- append-only audit log. Replaces the shared development admin bearer.

CREATE FUNCTION arknet_distinct_count(items text[]) RETURNS integer
    LANGUAGE sql IMMUTABLE STRICT
    AS $$ SELECT count(DISTINCT v)::integer FROM unnest(items) AS v $$;

CREATE TABLE administrators (
    id bigserial PRIMARY KEY,
    public_id text NOT NULL UNIQUE CHECK (public_id ~ '^admin_[0-9a-f]{64}$'),
    name text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9._-]{2,63}$' AND name NOT LIKE 'user\_%'),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Only the SHA-256 digest of a token is ever stored.
CREATE TABLE admin_tokens (
    id bigserial PRIMARY KEY,
    public_id text NOT NULL UNIQUE CHECK (public_id ~ '^admtok_[0-9a-f]{64}$'),
    admin_id bigint NOT NULL REFERENCES administrators(id),
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    permissions text[] NOT NULL CHECK (
        cardinality(permissions) BETWEEN 1 AND 4
        AND permissions <@ ARRAY['nodes.decide', 'users.admin', 'media.admin', 'relays.read']::text[]
        AND arknet_distinct_count(permissions) = cardinality(permissions)),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '90 days'),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

CREATE TABLE admin_audit_log (
    id bigserial PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    admin_id text NOT NULL REFERENCES administrators(public_id),
    token_id text NOT NULL REFERENCES admin_tokens(public_id),
    permission text CHECK (permission IS NULL OR permission IN ('nodes.decide', 'users.admin', 'media.admin', 'relays.read')),
    action text NOT NULL CHECK (action ~ '^[A-Za-z0-9 /{}_.-]{1,128}$'),
    target text NOT NULL DEFAULT '' CHECK (target = '' OR target = 'invalid' OR target ~ '^[a-z]{1,8}_[0-9a-f]{64}$'),
    outcome text NOT NULL CHECK (outcome IN ('denied', 'authorized', 'completed', 'token_minted', 'token_revoked')),
    status integer NOT NULL DEFAULT 0 CHECK (status BETWEEN 0 AND 599)
);

CREATE INDEX admin_audit_log_token ON admin_audit_log (token_id, at);

CREATE FUNCTION arknet_admin_audit_append_only() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN RAISE EXCEPTION 'admin_audit_log is append-only'; END $$;

CREATE TRIGGER admin_audit_log_no_update_delete BEFORE UPDATE OR DELETE ON admin_audit_log
    FOR EACH ROW EXECUTE FUNCTION arknet_admin_audit_append_only();
CREATE TRIGGER admin_audit_log_no_truncate BEFORE TRUNCATE ON admin_audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION arknet_admin_audit_append_only();
