CREATE TABLE node_identity (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    public_id text NOT NULL
);

CREATE TABLE node_configuration (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    settings jsonb NOT NULL CHECK (jsonb_typeof(settings) = 'object')
);

CREATE TABLE node_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    state text NOT NULL
);

CREATE TABLE core_enrollment (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    status text NOT NULL,
    core_url text,
    enrolled_at timestamptz
);
