# Core Enrollment Administration

Enrollment administration is separate from the public federation request API.
It is authorized by scoped, per-administrator tokens that are minted on the
Core host. Every route on this page requires the `nodes.decide` permission,
and every call is audited. See [Core administration](core-administration.md)
for minting, permissions, the audit log and Node capabilities. No Core admin
UI is implemented.

Missing or invalid authorization returns HTTP 401 before routing, including
for unknown administrative paths. A token without `nodes.decide` gets 403 and
no repository is touched. Credentials must be in one `Authorization: Bearer`
header; query parameters are never used for authentication. Administrative
responses use `Cache-Control: no-store`.

## Endpoints

### List pending nodes

```http
GET /api/admin/v1/nodes/pending?limit=50&cursor=<next_cursor>
Authorization: Bearer <token>
```

`limit` defaults to 50 and must be between 1 and 100. Omit `cursor` on the first
page; subsequent pages use the prior response's `next_cursor`. Results are
ordered by opaque node ID and contain only `PENDING` / `UNTRUSTED` records.
Unknown, duplicate, and malformed query parameters return HTTP 400.

```json
{
  "nodes": [],
  "next_cursor": ""
}
```

Each node has `node_id`, `version`, `created_at`, `enrollment_status`, and
`trust_status`. Internal numeric IDs and identifying metadata are not exposed.
An empty `next_cursor` ends pagination; `nodes` is an array even when empty.
Pagination is a live view, not a frozen snapshot across concurrent submissions.

### Approve a pending node

```http
POST /api/admin/v1/nodes/<opaque-node-id>/approve
Authorization: Bearer <token>
```

No request body or query parameters. Success returns HTTP 200 with the node ID,
`enrollment_status: APPROVED`, and `trust_status: TRUSTED`.

### Reject a pending node

```http
POST /api/admin/v1/nodes/<opaque-node-id>/reject
Authorization: Bearer <token>
```

No request body or query parameters. Success returns HTTP 200 with the node ID,
`enrollment_status: REJECTED`, and `trust_status: BLOCKED`. The row is retained;
rejection is a persisted decision, not deletion of the request.

Both actions use conditional atomic PostgreSQL updates. Only a currently
`PENDING` / `UNTRUSTED` node is eligible. Concurrent conflicting actions have
one winner; duplicate and opposite actions cannot overwrite that decision.
ID, version, creation time, and last-seen metadata remain unchanged. Existing
schema columns already support these states, so no migration rewrite is needed.

## Safe errors and persistence

- **400:** malformed opaque node ID, pagination, or action parameters/body.
- **401:** missing, unknown, expired or revoked administrator token.
- **403:** valid token without the `nodes.decide` permission.
- **404:** unknown route or missing node.
- **405:** authenticated request using an unsupported method.
- **409:** duplicate/opposite action or any noneligible existing node state.
- **503:** unavailable repository/database; raw driver details are not returned.

Approval and rejection survive Core restart and container recreation because
PostgreSQL is authoritative. Repeated public enrollment requests cannot undo
either decision; they return HTTP 409 for decided records. Do not delete the
Core database volume to restart the application.

Approval updates Core trust. The signed node handshake lets an Agent prove its
pinned-key ownership and learn this approval outbound, then persist `CONNECTED`.
Core does not issue downloadable private credentials, operator identity, or mesh
sessions. Legacy approvals without a pinned key fail closed for authentication.

## Examples

Mint a short-lived token and keep it out of shell history:

```sh
TOKEN=$(cd deploy/core && docker compose exec -T arknet-core arknet-core admin create-token \
  --name "$USER" --permission nodes.decide --ttl 1h | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
# For the default local Core port:
curl --fail -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8083/api/admin/v1/nodes/pending
# Set NODE_ID to the exact opaque node_id returned by the listing.
curl --fail -X POST -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:8083/api/admin/v1/nodes/$NODE_ID/approve"
# Or reject a different pending node:
curl --fail -X POST -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:8083/api/admin/v1/nodes/$NODE_ID/reject"
unset TOKEN
```

## Verification

From the repository root:

```sh
(cd core && go test ./... && go test -race ./... && go vet ./...)
python3 scripts/check-core-compose.py
python3 scripts/check-core-admin-stack.py
```

The live check uses a fresh uniquely named Core/PostgreSQL 18 project, generated
credentials, and loopback ports. It verifies authorization, complete pagination,
trust updates, retained rejection, invalid and duplicate actions, simultaneous
competing decisions, restart/recreation persistence, public re-enrollment
conflicts, scoped-token denial, audit attribution, revocation, refusal of the
removed dev-bearer setting, and the real PostgreSQL test suite. Only its
own containers, volumes, network, and Go helper image are removed; no development
`.env`, existing services, or developer database is changed. Run live stack checks
sequentially on constrained Docker VMs.

## Completed enrollment handshake

The Agent now persists a unique Ed25519 credential, signs enrollment and its own
approval-status requests, and learns Core approval outbound. Approval completion
atomically records federation enrollment and `CONNECTED`; completed enrollment
and credentials survive restart without enrollment traffic. This supersedes the
request-only startup behavior described in the earlier implementation phase.
See [completed enrollment handshake](node-enrollment-handshake.md)
for protocol, legacy keyless-approval safety, storage and two-node verification.
Operator identity, mesh sessions and a Core admin UI remain outside this phase.
