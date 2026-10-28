CREATE TABLE node_federation_credential (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    node_public_id text NOT NULL CHECK (node_public_id ~ '^node_[0-9a-f]{64}$'),
    private_seed bytea NOT NULL CHECK (octet_length(private_seed) = 32)
);

CREATE FUNCTION reject_federation_credential_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'federation credential is immutable';
END;
$$;
CREATE TRIGGER immutable_federation_credential
BEFORE UPDATE ON node_federation_credential
FOR EACH ROW EXECUTE FUNCTION reject_federation_credential_update();
