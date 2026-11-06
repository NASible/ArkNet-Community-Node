# Core Administration: Scoped Tokens, Audit and Node Capabilities

Prompt 49 (ARKSEC-005) replaced the shared development admin bearer with
per-administrator, route-scoped API tokens, an append-only audit log, and
per-Node capabilities. Decisions D6–D9 are recorded in
[the Phase 20 prompt pack](development/hermes-phase20-prompts.md).

## Administrator tokens

Tokens are minted only from a shell on the Core host. No HTTP route mints,
lists or extends them, and there is no browser login.

```sh
# Inside the Core Compose project (local dev: deploy/core):
docker compose exec -T arknet-core arknet-core admin create-token \
  --name alice --permission nodes.decide --permission users.admin --ttl 7d
```

`create-token` prints one JSON object to stdout and nothing else:

```json
{"token":"arknet_adm_…","token_id":"admtok_…","admin_id":"admin_…","admin":"alice",
 "permissions":["nodes.decide","users.admin"],"expires_at":"2026-11-12T00:00:00Z"}
```

- The token is shown once. Core stores only its SHA-256 digest. The
  `arknet_adm_` prefix lets secret scanners recognise it.
- `--name` identifies the administrator: 3–64 characters from `a-z 0-9 . _ -`.
  Tokens minted under the same name share one administrator identity.
- `--permission` is required and may be repeated. Unknown or repeated values
  are refused.
- `--ttl` is a Go duration (`12h`) or whole days (`7d`). The default is 30
  days, the minimum 1 minute, the maximum 90 days. The database enforces the
  maximum as well.
- `revoke-token --token-id admtok_…` revokes a token immediately.
- Exit codes are 0 for success, 1 for an operational failure, 2 for a usage
  error. Usage errors mint nothing.

Send the token as exactly one `Authorization: Bearer arknet_adm_…` header.
Query-string credentials are never accepted.

### Removed settings

`ARKNET_CORE_DEV_ADMIN_ENABLED` and `ARKNET_CORE_DEV_ADMIN_TOKEN` no longer
exist. If either is set, Core refuses to start and points to
`arknet-core admin create-token`. It does not echo the value. This stops a
stale deployment from assuming it is still protected by a bearer that no
longer exists.

## Permissions

| Permission | Routes |
| --- | --- |
| `nodes.decide` | `GET /api/admin/v1/nodes/pending`, `GET /api/admin/v1/nodes/{id}`, `POST …/{id}/approve`, `…/reject`, `…/revoke`, `PUT …/{id}/capabilities` |
| `users.admin` | `POST /api/admin/v1/users`, `POST …/users/{id}/disable`, `…/enable` |
| `media.admin` | `POST /api/admin/v1/media/{id}/remove` |
| `relays.read` | `GET /api/admin/v1/relays` |

Every admin route is registered with exactly one required permission. A test
(`TestAdminRoutePermissionRegistryIsExact`) fails if a route is added without
one.

Responses:

| Status | Meaning |
| --- | --- |
| 401 | Missing, malformed, unknown, expired or revoked token. Returned before routing. No audit row, because there is no attributable principal. |
| 403 | Valid token without the route's permission. A `denied` audit row is written. No repository is called. |
| 503 | The token store or audit log is unavailable. The action does not run. |

## Audit log

`admin_audit_log` records:

- for each authorized call: an `authorized` row before the action runs, then
  a `completed` row with the HTTP status;
- for each wrong-permission call: a `denied` row;
- `token_minted` and `token_revoked` rows from the CLI.

Each row holds the administrator ID, token ID, permission, route pattern (for
example `POST /api/admin/v1/nodes/{node_id}/approve`), target ID, outcome,
status and time. It never holds a secret, token, request body or password.
The schema limits the target to an opaque `prefix_<64 hex>` ID.

If the `authorized` row cannot be written, the action does not run and the
caller gets 503. Triggers reject `UPDATE`, `DELETE` and `TRUNCATE` on the
table.

To list recent actions with administrator names:

```sql
SELECT l.at, a.name, l.token_id, l.permission, l.action, l.target, l.outcome, l.status
FROM admin_audit_log l JOIN administrators a ON a.public_id = l.admin_id
ORDER BY l.id DESC LIMIT 50;
```

## Node capabilities

Each Node signs requests with one enrollment key (D9: no second key). Core
limits what that key may do, per Node and per route:

| Capability | Routes |
| --- | --- |
| *(base, always)* | enrollment status, heartbeats, catalog, media misses |
| `memberships` | invitations, offline authorizations and refresh, login callbacks |
| `swarm` | tracker grants, replica reports, the relay list |

New and existing Nodes get both capabilities by default, so upgrades do not
change behaviour. To narrow a Node (an empty list leaves base routes only):

```sh
curl --fail -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"capabilities":["swarm"]}' \
  "http://127.0.0.1:8083/api/admin/v1/nodes/$NODE_ID/capabilities"
```

The body must be exactly `{"capabilities": [...]}`. A Node calling a route it
lacks the capability for gets 403 after signature verification and before any
repository is touched. `GET /api/admin/v1/nodes/{id}` shows the current set.

## Scripts

- Harnesses mint a least-privilege token for each run with
  `scripts/core_admin.py` (`ScopedAdmin`), using the CLI inside the Core
  container. The MVP E2E uses a separate token for each of `nodes.decide`,
  `users.admin` and `media.admin`.
- The live-test scripts mint tokens over SSH on the Core VM
  (`ops.mint_core_admin`) and revoke them when the run ends. No admin token is
  stored in `secrets/runtime.json` or deployed to any VM.

## Verification

```sh
(cd core && go test ./... && go vet ./...)
python3 scripts/check-core-admin-stack.py
```

The stack check verifies wrong-scope 403s with unchanged state, attributed
audit rows, tokens absent from responses, logs and a full database dump,
immediate revocation, and that a stale dev-bearer setting stops Core.
