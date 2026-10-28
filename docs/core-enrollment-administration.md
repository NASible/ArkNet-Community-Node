# Core Enrollment Administration

Enrollment administration is separate from the public federation request API.
It is disabled by default and currently uses an **explicitly development-only**
Bearer token. No Core admin UI or real identity provider is implemented. The
separate node handshake now provides node-bound credentials and outbound approval
checks; it does not implement operator identity.

## Development authorization

Set these in your existing `deploy/core/.env` for local development only:

```dotenv
ARKNET_CORE_DEV_ADMIN_ENABLED=true
ARKNET_CORE_DEV_ADMIN_TOKEN=<generated token>
```

Generate the token locally:

```sh
python3 -c 'import secrets; print(secrets.token_hex(32))'
```

The token must be exactly 64 lowercase hexadecimal characters. Malformed tokens
are rejected, not trimmed or normalized. There is no default or embedded token.
Setting a token without explicit enablement fails configuration validation.
Errors and application logs do not echo the token. Never commit it, send it to
an Agent, put it in a URL, or expose this development API publicly. Keep the
Core Compose port bound to loopback. This shared token is not production
authentication and provides no individual administrator identity or audit trail.

Rebuild the existing configured Core stack:

```sh
(cd deploy/core && docker compose up --build -d --wait)
```

When disabled, administrative routes return HTTP 404. When enabled, missing or
invalid authorization returns HTTP 401 before repository access, including for
unknown administrative paths and unsupported methods. Credentials must be in
one `Authorization: Bearer <token>` header; query parameters are never used for
authentication. Administrative responses use `Cache-Control: no-store`.

`adminAuthorizer` in `core/admin_auth.go` is the replaceable authorization boundary.
Routing and repositories do not inspect development credentials. A real
identity/permission verifier can replace that implementation without changing
enrollment handlers or the storage operations.

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
- **401:** enabled administrative API without valid authorization.
- **404:** disabled administrative API, unknown route, or missing node.
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

Supply the token from your local secret handling, rather than committing it or
placing it in shell history as a literal:

```sh
read -r -s CORE_ADMIN_TOKEN
export CORE_ADMIN_TOKEN
# For the default local Core port:
curl --fail -H "Authorization: Bearer $CORE_ADMIN_TOKEN" \
  http://127.0.0.1:8083/api/admin/v1/nodes/pending
# Set NODE_ID to the exact opaque node_id returned by the listing.
curl --fail -X POST -H "Authorization: Bearer $CORE_ADMIN_TOKEN" \
  "http://127.0.0.1:8083/api/admin/v1/nodes/$NODE_ID/approve"
# Or reject a different pending node:
curl --fail -X POST -H "Authorization: Bearer $CORE_ADMIN_TOKEN" \
  "http://127.0.0.1:8083/api/admin/v1/nodes/$NODE_ID/reject"
unset CORE_ADMIN_TOKEN
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
conflicts, disabled-mode isolation, and the real PostgreSQL test suite. Only its
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
