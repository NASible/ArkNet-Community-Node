-- Core-bound Node login (Prompt 46A). A Node registers the callback URLs Core
-- may redirect a browser to after login, over its signed federation channel.
CREATE TABLE node_login_callbacks (
    node_id bigint NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    position smallint NOT NULL CHECK (position BETWEEN 1 AND 4),
    callback_url text NOT NULL CHECK (char_length(callback_url) BETWEEN 1 AND 512),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, position),
    UNIQUE (node_id, callback_url)
);

-- At most one assertion per (Node, nonce). Only the SHA-256 of the nonce is
-- stored, and only until it can no longer matter.
CREATE TABLE node_login_nonces (
    node_id bigint NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    nonce_digest bytea NOT NULL CHECK (octet_length(nonce_digest) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at),
    PRIMARY KEY (node_id, nonce_digest)
);

CREATE INDEX node_login_nonces_expires_at ON node_login_nonces (expires_at);
