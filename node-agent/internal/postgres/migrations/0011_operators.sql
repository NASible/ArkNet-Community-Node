-- Node operator accounts (docs/node-operator-authentication.md). Stored only in
-- this Node's database and never sent to Core. Codes and tokens are stored as
-- SHA-256 digests; passwords as argon2id hashes. The TOTP secret must be
-- readable to verify codes and is protected by database access.
CREATE TABLE operators (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id text NOT NULL UNIQUE CHECK (public_id ~ '^op_[0-9a-f]{64}$'),
    username text NOT NULL UNIQUE CHECK (username ~ '^[a-z0-9][a-z0-9._-]{2,31}$'),
    status text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DISABLED')),
    password_hash text NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    totp_secret bytea NOT NULL CHECK (octet_length(totp_secret) = 20),
    totp_last_step bigint NOT NULL DEFAULT 0 CHECK (totp_last_step >= 0),
    failed_logins integer NOT NULL DEFAULT 0 CHECK (failed_logins >= 0),
    retry_after timestamptz,
    last_sign_in_at timestamptz,
    credentials_changed_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- SETUP codes create an operator; RECOVERY codes are bound to one operator.
CREATE TABLE operator_setup_codes (
    code_digest bytea PRIMARY KEY CHECK (octet_length(code_digest) = 32),
    purpose text NOT NULL CHECK (purpose IN ('SETUP', 'RECOVERY')),
    operator_id bigint REFERENCES operators (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    CHECK ((purpose = 'SETUP') = (operator_id IS NULL)),
    CHECK (expires_at > created_at)
);

-- A redeemed code waiting for TOTP confirmation.
CREATE TABLE operator_enrollments (
    token_digest bytea PRIMARY KEY CHECK (octet_length(token_digest) = 32),
    purpose text NOT NULL CHECK (purpose IN ('SETUP', 'RECOVERY')),
    operator_id bigint REFERENCES operators (id) ON DELETE CASCADE,
    username text NOT NULL CHECK (username ~ '^[a-z0-9][a-z0-9._-]{2,31}$'),
    password_hash text NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    totp_secret bytea NOT NULL CHECK (octet_length(totp_secret) = 20),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK ((purpose = 'SETUP') = (operator_id IS NULL)),
    CHECK (expires_at > created_at)
);

CREATE TABLE operator_recovery_codes (
    operator_id bigint NOT NULL REFERENCES operators (id) ON DELETE CASCADE,
    code_digest bytea NOT NULL CHECK (octet_length(code_digest) = 32),
    used_at timestamptz,
    PRIMARY KEY (operator_id, code_digest)
);

CREATE TABLE operator_sessions (
    token_digest bytea PRIMARY KEY CHECK (octet_length(token_digest) = 32),
    operator_id bigint NOT NULL REFERENCES operators (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    authenticated_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    idle_expires_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at),
    CHECK (idle_expires_at <= expires_at)
);

CREATE INDEX operator_sessions_operator_id ON operator_sessions (operator_id);

-- Local audit trail. Never holds secrets and is never sent to Core.
CREATE TABLE operator_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    operator_id bigint REFERENCES operators (id) ON DELETE SET NULL,
    event text NOT NULL CHECK (event IN ('SETUP_CODE_ISSUED', 'RECOVERY_CODE_ISSUED', 'ENROLLMENT_STARTED',
        'OPERATOR_CREATED', 'OPERATOR_RECOVERED', 'SIGNED_IN', 'SIGN_IN_FAILED', 'RECOVERY_CODE_USED',
        'SIGNED_OUT', 'OPERATOR_DISABLED'))
);
