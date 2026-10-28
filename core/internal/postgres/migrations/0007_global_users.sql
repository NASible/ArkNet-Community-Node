-- ArkNet-owned global user accounts. No plaintext passwords or session tokens
-- are stored: only argon2id PHC strings and SHA-256 token digests.
CREATE TABLE global_users (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id text NOT NULL UNIQUE CHECK (public_id ~ '^user_[0-9a-f]{64}$'),
    username text NOT NULL UNIQUE CHECK (username ~ '^[a-z0-9][a-z0-9._-]{2,31}$'),
    password_hash text NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    status text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DISABLED')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE global_user_sessions (
    token_digest bytea PRIMARY KEY CHECK (octet_length(token_digest) = 32),
    user_id bigint NOT NULL REFERENCES global_users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at)
);

CREATE INDEX global_user_sessions_user_id ON global_user_sessions (user_id);
