-- Prompt 49 (ARKSEC-005): per-Node key authority beyond the always-on base
-- routes. Existing and new Nodes keep both groups by default; an administrator
-- narrows them with PUT /api/admin/v1/nodes/{node_id}/capabilities.
ALTER TABLE nodes
    ADD COLUMN capabilities text[] NOT NULL DEFAULT ARRAY['memberships', 'swarm']::text[]
    CHECK (capabilities <@ ARRAY['memberships', 'swarm']::text[]
           AND arknet_distinct_count(capabilities) = cardinality(capabilities));
