-- One primary home node per global user, stored only in Core.
-- The composite foreign key guarantees the home node is one of the user's own
-- memberships; removing that membership removes the home-node choice rather
-- than silently selecting another Node.
ALTER TABLE node_memberships
    ADD CONSTRAINT node_memberships_id_user_id_key UNIQUE (id, user_id);

CREATE TABLE global_user_home_nodes (
    user_id bigint PRIMARY KEY REFERENCES global_users (id) ON DELETE CASCADE,
    membership_id bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (membership_id, user_id) REFERENCES node_memberships (id, user_id) ON DELETE CASCADE
);

CREATE INDEX global_user_home_nodes_membership_id ON global_user_home_nodes (membership_id);
