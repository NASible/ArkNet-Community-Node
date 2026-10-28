# Primary Home Node

Each global user may belong to several Nodes but has at most one primary home
node. The relationship is stored only in Core and is used by Core routing
logic. Community Nodes never receive it.

## Storage

Migration `0010_home_nodes.sql` adds `global_user_home_nodes`:

- `user_id` is the primary key, so a user has at most one home node;
- `membership_id` references the user's own membership through a composite
  foreign key on `(membership_id, user_id)`, so the database rejects a home
  node that is another user's membership;
- only internal keys are stored. Node and user public IDs are not copied in.

Removing the home membership deletes the choice (`ON DELETE CASCADE`). Core
never silently promotes another membership.

## Explicit change

The user it belongs to reads or changes it with a Core session:

```text
GET /api/identity/v1/home-node
PUT /api/identity/v1/home-node        {"node_id":"node_…"}
Authorization: Bearer <Core session token>
```

Both return only `{"node_id","set_at"}` (`404` from GET if no home node is set).
`PUT` is the only writer:

- accepting an invitation, joining more Nodes, or an operator action never sets
  or changes the home node;
- the target must be one of the caller's own memberships, on an approved and
  trusted Node, for an active user;
- not being a member, an unknown Node, and an untrusted Node all return the
  same `404 home node is unavailable`, so the endpoint cannot be used to probe
  Nodes;
- the body must be exactly `{"node_id": "<valid node ID>"}`. A Node signature is
  not a user session and is rejected.

## Routing access

Core routing logic gets the home node through the read-only `homeNodeSource`
interface. `preferredHomeNode` resolves an authenticated Core session to its
global user, then returns that user's `HomeNode{NodeID, MemberID, SetAt}`.
`MemberID` is the pairwise pseudonym valid only on the home Node. No other
membership is loaded. A lookup reports no home node when the user is disabled,
or when the home Node is no longer approved and trusted. [Media routing](media-routing.md) uses this interface.

## Privacy boundary

No federation (Node-signed) endpoint reads `global_user_home_nodes`. Every Node,
including the home Node, has no bulk membership enumeration route. It gets no
home designation, no other Node IDs, no pseudonyms from other Nodes, and no Core
user ID or username. Pairwise IDs from another Node don't resolve through
offline authorization or refresh (they return `Authorized: false` or a generic
denial).

## Verification

- `core/home_nodes_test.go`: explicit change, no implicit selection, a uniform
  non-member error, strict input, rejection of Node signatures, the routing
  accessor, and a `404` for Node membership enumeration.
- `core/internal/postgres/home_nodes_test.go` (real PostgreSQL): one row per
  user, set/change, the composite foreign key, trust and disabled-user
  filtering, no fallback after the home membership is deleted, and cross-Node
  pseudonyms not resolving.
- Mutation checks: removing the trust filter from the lookup, or the
  own-membership check from the setter, makes these tests fail.
- Live run (2026-10-25): one Core and three real Agents. Alice joined A and B,
  Bob joined A and C. No home node existed until an explicit `PUT`. Alice's home
  moved B → A → B. Exactly one Core row was stored. Non-member and unknown Node
  responses were byte-identical. Joining C later didn't change the home node.
  The non-home Node databases held no other Node IDs, foreign pseudonyms, Core
  user ID, or home data.
