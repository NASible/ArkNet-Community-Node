-- Node membership invitations name no user. Only the SHA-256 digest of the
-- invitation secret is stored; creation_proof is a digest of the signed Node
-- request, unique so a replayed signed request cannot mint another invitation.
CREATE TABLE node_invitations (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id text NOT NULL UNIQUE CHECK (public_id ~ '^invite_[0-9a-f]{64}$'),
    node_id bigint NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    secret_digest bytea NOT NULL UNIQUE CHECK (octet_length(secret_digest) = 32),
    creation_proof bytea NOT NULL UNIQUE CHECK (octet_length(creation_proof) = 32),
    role text NOT NULL CHECK (role IN ('MEMBER', 'ADMIN')),
    max_uses integer NOT NULL CHECK (max_uses BETWEEN 1 AND 100),
    uses integer NOT NULL DEFAULT 0 CHECK (uses >= 0 AND uses <= max_uses),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);

CREATE INDEX node_invitations_node_id ON node_invitations (node_id);

CREATE TABLE node_memberships (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    node_id bigint NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES global_users (id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('MEMBER', 'ADMIN')),
    invitation_id bigint REFERENCES node_invitations (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (node_id, user_id)
);

CREATE INDEX node_memberships_user_id ON node_memberships (user_id);
