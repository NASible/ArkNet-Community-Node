# Media Availability Routing

Core tells an authenticated global user where to fetch an active ArkNet media
object: their home node, or Core.

```text
GET /api/routing/v1/media/{media_id}
Authorization: Bearer <Core session token>
```

## Decision

1. Look up the caller's [primary home node](home-node.md). With no usable home
   node, select Core.
2. **Is the home node healthy?** All of these must hold:
   - it is `APPROVED` and `TRUSTED`;
   - its last accepted heartbeat state is `CONNECTED`, `SYNCHRONIZING`, or
     `MESH`;
   - that heartbeat reported both `healthy` and `database_healthy`;
   - Core accepted it within `routing.MaxHeartbeatAge` (90 s, measured by
     Core's own clock).
3. **Does the home node report verified local availability?** It must hold a
   Core replica record (written only after a signed, hash- and size-checked
   replica report) for an object that is still `ACTIVE`.
4. If both are true, prefer the home node. Otherwise select Core.

Only the home node is considered. A copy on another Node the user belongs to
doesn't change the decision, and Peer Serving doesn't apply: it governs serving
to other Nodes, not a user's own home node.

## Response

Only the minimum is returned:

```json
{"target":"HOME_NODE","node_id":"node_…"}
{"target":"CORE"}
```

No reason, health detail, timestamps, replica list, member ID, or other Node
identity is included. A user on Core can't tell "media missing" from "home node
offline". `node_id` is only ever the caller's own home node. Errors:

- `401` with no valid session. Authentication runs before any catalog read.
- `400` for a malformed ID or any query parameter.
- `404 media unavailable` for an unknown or `REMOVED` object.
- `503 routing unavailable` on internal failure. No database detail is exposed.

No Node is contacted during a decision. A Core fallback caused by a missing
object is recorded afterward as an anonymous [media-miss event](media-miss-events.md),
without delaying the response. Routing uses only facts Core
already has: accepted heartbeats and replica reports.

## Limits

- Agents with a heartbeat interval above about 90 s always route to Core, since
  their heartbeats are older than the freshness bound. The safe direction is
  intentional.
- After a home node goes offline, routing can keep pointing at it for up to
  90 s, until its last heartbeat ages out.
- A replica record means the Node verified and reported the object. Core isn't
  told if an operator later deletes an active object's file outside ArkNet. The
  `REMOVED` path from Prompt 33 is handled.

## Verification

- `core/internal/routing/routing_test.go`: the decision table.
- `core/routing_decisions_test.go`: the three acceptance cases, no home node,
  only the home node inspected, minimal response keys, 404/503 handling, and
  session checked before catalog access.
- `core/internal/postgres/routing_test.go` (real PostgreSQL): health by state,
  health flags, freshness, and trust; availability by verified replica; removed
  objects never available; an unknown Node.
- Mutation checks: removing the freshness bound, the state check, the
  active-object check, the health gate, or the availability gate each fail
  the tests.
- Live run (2026-10-25), with real Core and two Agents. Alice belonged to A and
  B with home B. B held object X and A held object Y:
  - X routed to `HOME_NODE`/B;
  - Y routed to `CORE`, even though A had it;
  - after Agent B was stopped, X routed to `CORE` within 90 s;
  - after B restarted, X returned to B;
  - once X was removed, routing returned `404`;
  - Agent logs showed no routing traffic.
