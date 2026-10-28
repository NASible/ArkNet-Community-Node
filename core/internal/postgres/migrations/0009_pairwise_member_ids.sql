-- Pairwise membership IDs: one random pseudonym per (global user, Node) pair.
-- Nodes receive only this ID; the mapping to global_users stays in Core.
ALTER TABLE node_memberships ADD COLUMN public_id text;

-- Backfill pre-existing memberships with independent random values.
-- gen_random_uuid() uses PostgreSQL's strong random source; hashing two of
-- them yields 64 uniformly distributed hex characters (no pgcrypto needed).
UPDATE node_memberships
SET public_id = 'member_' || encode(sha256(uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid())), 'hex');

ALTER TABLE node_memberships
    ALTER COLUMN public_id SET NOT NULL,
    ADD CONSTRAINT node_memberships_public_id_key UNIQUE (public_id),
    ADD CONSTRAINT node_memberships_public_id_check CHECK (public_id ~ '^member_[0-9a-f]{64}$');
