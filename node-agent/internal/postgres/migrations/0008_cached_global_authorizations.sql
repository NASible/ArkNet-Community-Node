-- Bounded Node-side offline verification for previously authorized global
-- members. No Core user ID, username, plaintext password, or Core password hash
-- is stored. password_hash is freshly salted by this Node after online login.
CREATE TABLE cached_global_authorizations (
    member_id text PRIMARY KEY CHECK (member_id ~ '^member_[0-9a-f]{64}$'),
    node_id text NOT NULL CHECK (node_id ~ '^node_[0-9a-f]{64}$'),
    password_hash text NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    role text NOT NULL CHECK (role IN ('MEMBER', 'ADMIN')),
    permissions text[] NOT NULL CHECK (permissions = ARRAY['MEDIA_ACCESS']::text[]),
    validated_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > validated_at),
    CHECK (expires_at <= validated_at + interval '24 hours')
);

CREATE TABLE cached_global_sessions (
    token_digest bytea PRIMARY KEY CHECK (octet_length(token_digest) = 32),
    member_id text NOT NULL REFERENCES cached_global_authorizations (member_id) ON DELETE CASCADE,
    permissions text[] NOT NULL CHECK (permissions = ARRAY['MEDIA_ACCESS']::text[]),
    offline boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at)
);

CREATE INDEX cached_global_sessions_member_id ON cached_global_sessions (member_id);
