# Pairwise membership IDs

Every global user ↔ Node membership has its own pseudonymous **membership ID**:

```text
member_<64 lowercase hex>
```

It is the only identifier for a global user that a Node ever receives.

## Properties

- **Pairwise:** one ID per (global user, Node) pair. User A on Node 1 and on
  Node 2 has two unrelated IDs, so Nodes, even colluding ones, cannot correlate
  a user across Nodes by identifier.
- **Random, not derived:** each ID is 256 bits from `crypto/rand`, generated
  when the membership is created. It is deliberately *not* an HMAC or hash of
  the user ID and Node ID. No derivation key exists that could leak, and no
  computation by a Node can reverse an ID to the global user.
- **Stable:** the ID does not change for the life of the membership.
- **Private mapping:** only Core's `node_memberships` table links `public_id` to
  `global_users`. That mapping is never exposed through any Node-facing API.
- **No global user ID to Nodes:** Node-facing responses contain no `user_…` ID
  and no username. Core resolves only a caller-supplied pairwise member ID for a
  single authorization operation; there is no Node-facing bulk membership list.

## Where the ID appears

| Consumer | Endpoint | Fields |
| --- | --- | --- |
| The accepting user | `POST /api/identity/v1/invitations/accept` | `node_id`, `member_id`, `role`, `joined_at` |
| The Node (signed, per-member) | `POST /api/federation/v1/nodes/{node_id}/offline-authorizations` | one caller-supplied `member_id`; Node-scoped authorization result |
| The Node (signed, cached IDs only) | `POST /api/federation/v1/nodes/{node_id}/offline-authorizations/refresh` | bounded caller-supplied `member_ids`; authorization state only |

Core never provides a discovery or listing endpoint for membership IDs. A Node
can resolve only an ID already presented by that member or already held in its
bounded authorization cache. Unknown, inactive, and wrong-Node IDs receive the
same denial.

## Migration

Migration `0009_pairwise_member_ids.sql` adds the column and backfills
pre-existing memberships with independent random IDs: SHA-256 over two
`gen_random_uuid()` values, which come from PostgreSQL's strong random source,
so no extension is needed. It then enforces `NOT NULL`, `UNIQUE`, and the ID
format.

## Not in this phase

Paginated listing beyond 1000 members, membership removal, Node-side
persistence or synchronization of members, and offline verification material
([Prompt 32](development/hermes-mvp-prompts.md)).

## Verification

- **PostgreSQL tests:**
  - the same user gets different, unrelated IDs on two Nodes;
  - Core holds the mapping;
  - per-member lookups return no global user data and reject cross-Node IDs;
  - uniqueness and format constraints are enforced;
  - the migration backfills randomly.
- **HTTP tests:** the removed bulk membership route returns `404`; signed
  per-member authorization accepts only the target Node's presented IDs.
- **Mutation checks:** deriving the ID from the user ID or weakening Node scope
  makes these tests fail.
