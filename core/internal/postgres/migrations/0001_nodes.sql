CREATE TABLE nodes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id text NOT NULL UNIQUE CHECK (public_id ~ '^node_[0-9a-f]{64}$'),
    enrollment_status text NOT NULL DEFAULT 'PENDING' CHECK (btrim(enrollment_status) <> ''),
    trust_status text NOT NULL DEFAULT 'UNTRUSTED' CHECK (btrim(trust_status) <> ''),
    software_version text NOT NULL CHECK (btrim(software_version) <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz
);
