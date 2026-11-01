# ArkNet Node Agent

Go 1.25+ community-node agent with local PostgreSQL state. Uses the pinned pgx
PostgreSQL driver, following Core's existing database pattern. Outbound
enrollment, approval reconciliation, and authenticated heartbeat connectivity
are supported. The Agent has a signed, validated Core media-catalog fetch
boundary and reports health from a configured local Jellyfin; automatic catalog
scheduling and mesh network detection are not implemented.

## Run locally

Use the [Node development Compose stack](../deploy/node/README.md). With its
local `.env` configured, from `deploy/node/`:

```sh
docker compose up --build -d --wait
curl --fail http://127.0.0.1:8082/health
curl --fail http://127.0.0.1:8082/api/v1/status
```

Healthy response: `{"healthy":true,"database_healthy":true}`. Database ping
failure returns HTTP 503 with both fields false; the ping has a 2-second limit.
Status returns persisted `node_id`, `state`, `state_changed_at`, enrollment and
last-contact state, plus `version`, Jellyfin health, and heartbeat-observed
`core_connectivity` when federation is enabled. Disabled
federation retains `core_connectivity: "NOT_IMPLEMENTED"`. Database read failure returns HTTP 503
with `{"error":"local state database unavailable"}` instead of fabricated state.
No credentials or driver errors appear in HTTP responses.

Standalone `go run .` listens on `127.0.0.1:8081` for management and
`127.0.0.1:8082` for public/member routes, and requires a reachable PostgreSQL
database configured below. No Core service is needed.
The Compose database is deliberately not host-published. Local Compose explicitly
sets `PGSSLMODE=disable` on its dedicated development network only; do not copy
that setting to remote deployments. Passwords supplied through `PGPASSWORD`
are URL-encoded safely. Keep connection URLs and passwords out of logs.

## Persistent local state

The internal repository boundary is `internal/localstate.Repository`; PostgreSQL
implementation and embedded migrations live under `internal/postgres/`.
Startup connects, migrates, and initializes missing singleton rows transactionally
before serving HTTP. Concurrent startup is serialized by an advisory lock.
Applied migrations have checksum validation. Add forward migrations rather than
editing an already applied migration.

Local state tables are:

- `node_identity`: one opaque public node ID, generated from cryptographic randomness.
- `node_configuration`: local settings initialized to an empty JSON object.
- `node_state`: persisted typed state and its `changed_at` timestamp, initially `PROVISIONING`.
- `node_state_transitions`: atomically recorded state-transition history.
- `core_enrollment`: initial `NOT_ENROLLED` status and nullable Core URL/enrollment timestamp.
- `local_users`: Node-scoped random IDs, normalized usernames, argon2id hashes, and status.
- `local_user_sessions`: SHA-256 bearer-token digests and fixed expirations linked only to local users.

No Core administrator password, token, or other administrator secret is stored.
Local configuration is not a secret vault. Local user identities stay only in
`local_users` and never synchronize to Core. The safe enrollment state and last successful
Core-contact timestamp are exposed through status; credentials and Core URL are not.

Repeated initialization only creates missing rows; it never resets stored
identity, settings, state, or enrollment information. Invalid stored identity
fails rather than silently replacing it. A missing identity alongside existing
local rows also fails closed: restore the identity from backup rather than
silently assigning a new identity to an existing Node. Restarting or recreating only the Agent
container retains its identity because the identity lives in PostgreSQL, not in
the container filesystem. Deleting the database volume loses identity and state;
do not do so for routine maintenance. Restoring a cloned database also clones
its identity—do not run it as a second independent Node.

Startup database failure exits nonzero. A later outage marks health unhealthy;
health and status recover when PostgreSQL returns, without restarting Agent.
Most local state remains read-only; the local-user management endpoints are the
explicit exception. Opt-in outbound enrollment is described below.

## Environment configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `ARKNET_AGENT_ADDR` | `127.0.0.1:8081` | Management listener in `host:port` form; port `0` selects an available port. |
| `ARKNET_MEMBER_ADDR` | `127.0.0.1:8082` | Public/member listener in `host:port` form; port `0` selects an available port. |
| `ARKNET_AGENT_LOG_LEVEL` | `INFO` | Standard `slog` level: `DEBUG`, `INFO`, `WARN`, or `ERROR` (case-insensitive; numeric offsets are supported). |
| `ARKNET_AGENT_SHUTDOWN_TIMEOUT` | `10s` | Positive Go duration allowed for draining active requests. |
| `DATABASE_URL` | Unset | Overrides all `PG*` settings with a PostgreSQL URL. |
| `PGHOST` / `PGPORT` | `127.0.0.1` / `5432` | Database address. |
| `PGDATABASE` / `PGUSER` | `arknet_node` / `arknet` | Local database and role. |
| `PGPASSWORD` | Empty | Local database password; never commit it. |
| `PGSSLMODE` | `require` | TLS by default; use verified TLS for remote databases. |
| `JELLYFIN_API_URL` | Unset | Local Jellyfin base URL used only for the `/health` probe. |
| `ARKNET_RELEASE_INDEX_URL` | Unset | HTTPS channel index for read-only [update discovery](../docs/update-discovery.md); unset disables discovery. |
| `ARKNET_RELEASE_INDEX_ALLOW_HTTP` | `false` | Permit a non-loopback HTTP index (testing only). |
| `ARKNET_RELEASE_MANIFEST` | Unset | Clean absolute path of the current whole-stack release manifest; unset or invalid reports `release: null`. |

Unset or empty values use defaults. Invalid configuration or listener failures
exit with status 1. Core server variables such as `ARKNET_CORE_ADDR` do not
configure the Agent; `ARKNET_CORE_URL` specifically configures its outbound client.

Management routes require an operator session, and the two listeners split
routes by the permission matrix: wrong-listener routes answer 404. See
[operator authentication](../docs/node-operator-authentication.md#listener-split).
Keep both default loopback bindings; binding to all interfaces requires
appropriate external access controls. The [Node development Compose stack](../deploy/node/README.md)
binds the Agent on the container network while publishing both listeners only
on host loopback, and provides the persistent private PostgreSQL database.

Structured JSON logs go to stdout with startup address/version, shutdown events,
and errors. No user identities or request bodies are logged. HTTP timeouts match
Core: 5s for headers, 15s for reads, 30s for writes, and 60s for idle connections.

## Build, stop, and test

Within `node-agent/`:

```sh
go build -o bin/arknet-agent .
./bin/arknet-agent
```

Send SIGTERM to the compiled service process, or Ctrl-C (SIGINT). The Agent stops
accepting connections, drains active requests, logs `shutdown_complete`, and
exits with status 0. If the shutdown deadline expires, remaining connections
are closed, an error is logged, and it exits with status 1. Use the compiled
binary for signal verification instead of signaling the `go run` wrapper.

```sh
go test ./...
go test -race ./...
go vet ./...
```

Database integration tests require `ARKNET_TEST_DATABASE_URL` and skip explicitly
when absent. Run `python3 scripts/check-node-stack.py` from the repository root
to exercise isolated real PostgreSQL initialization and persistence tests.

Tests cover endpoint JSON and typed initial state, method and route handling,
version consistency, configuration defaults/overrides/validation, active-request
draining, shutdown deadlines, listener errors, and the generic application
adapter contract with Jellyfin as its first implementation.

## Local transfer boundary (mock only)

Set `ARKNET_TRANSFER_URL` and a distinct `ARKNET_TRANSFER_TOKEN` together to enable
the local mock transfer API. Both unset disables it. Plain HTTP is limited to
loopback and the exact `arknet-transfer` sidecar name; every other endpoint requires HTTPS.
Agent starts/resumes only
a selected object that still appears in its freshly authorized Core catalog;
pause/cancel work without Core. Only ID, size, and hash reach transfer, never
Core admin credentials or enrollment keys. GET `/api/v1/transfers` supplies the
UI with synthetic progress and terminal reports. Mock completion never marks
media `AVAILABLE`, and no automatic transfer starts on selection or restart.
See [API, errors, security, and opt-in deployment](../docs/transfer-service-interface.md).

## Local media selection

`GET /api/v1/media` combines the signed Core catalog with durable local
selections; `POST /api/v1/media/{media_id}/selection` queues an object using
Core-owned metadata. Configure `ARKNET_MEDIA_STORAGE_ALLOCATION_GIB` in whole
GiB (`0` by default disables new selections; `-1` means unlimited). Reservations are checked transactionally
and survive Agent restart. No automatic caching or downloads are started.
See [Node media selection](../docs/node-media-selection.md) for states, offline
behavior, local API security boundaries, and live verification.

## Core media catalog client

`internal/catalog.Fetch` lets an approved Agent retrieve the catalog from
`GET /api/federation/v1/nodes/{node_id}/media`. The outbound request uses the
existing node-specific Ed25519 enrollment credential. Responses are bounded and
validated before use, including opaque media IDs, type, non-negative size,
`ACTIVE`/`REMOVED` state, optional BitTorrent v2 info hash, and timestamps.

Removed records are retained as tombstones in the fetched list. The client does
not expose credentials or Core configuration in its result. This phase adds the
fetch boundary and tests, not periodic synchronization, local cache persistence,
downloads, or swarm participation. See [Core media catalog](../docs/media-catalog.md).

## Local application adapters

Local application integration is defined by `internal/application.Adapter`.
The interface covers identity, health, enabled/configured state, version, and
start/stop/restart lifecycle requests. Jellyfin-specific HTTP behavior lives in
`internal/application/jellyfin`; the status handler consumes only the generic
interface while retaining the compatible `jellyfin_healthy` response field.

This is explicit compile-time wiring, not a plugin marketplace. Lifecycle calls
require an injected `application.Lifecycle` controller. None is installed in
this phase, so the Agent does not receive the Docker socket and lifecycle calls
fail explicitly as unavailable; Compose remains responsible for starting and
restarting Jellyfin. See [local application adapters](../docs/application-adapters.md).

## Node-local users

Operators can create and manage local-only accounts through the **Users** page
or `/api/v1/local-users`. Accounts, argon2id password hashes, and session
digests stay exclusively in this Node's PostgreSQL database; local login works
with Core offline. The Users UI and API enumerate local accounts only; global
memberships are never exposed as an operator-facing list. See [Node-local users](../docs/local-users.md)
for API, security boundaries, and the live two-Node proof.

## Architecture boundary

Future work belongs here for cached access for authorized global users, offline
global authorization, and private swarm participation. Local users never synchronize to Core; global users use
pairwise pseudonymous IDs. Nodes are independently controlled, untrusted, and
primarily initiate connections outbound.

## Explicit state machine

The Agent validates and transactionally persists explicit lifecycle transitions.
See the [transition graph and persistence semantics](../docs/node-state-machine.md).
`REVOKED` is terminal; invalid and stale transitions return typed errors without
changing current state, timestamp, or history. No automatic network detection
or HTTP mutation route is added.

## Outbound enrollment

Set `ARKNET_CORE_URL` to an HTTPS Core base URL for signed enrollment at
startup while not yet approved, followed by outbound approval checks. Empty/unset disables submission and keeps standalone/offline development
independent of Core. `ARKNET_CORE_ALLOW_HTTP=true` is an explicit private-development
opt-in; loopback HTTP is also supported. See [enrollment flow and setup](../docs/node-enrollment.md).

The signed request includes persisted node ID, release version, empty capabilities,
and the public Ed25519 key; private credentials never leave local storage. Core
acknowledgment must match node ID and `PENDING` status before recording pending
enrollment and `ENROLLMENT_PENDING`. Repeated pending receipts preserve timestamps.
While pending, the Agent checks signed approval status every five seconds. It
atomically completes federation enrollment and persists `CONNECTED` only after
authenticated `APPROVED` / `TRUSTED`. Completed nodes skip enrollment traffic at
restart. No operator identity, certificate or Core administrator secret is added.

Run `python3 scripts/check-enrollment-stack.py` from repository root for a fully
isolated real Core/Agent enrollment and retry verification.

## Completed enrollment handshake

The Agent now persists a unique Ed25519 credential, signs enrollment and its own
approval-status requests, and learns Core approval outbound. Approval completion
atomically records federation enrollment and `CONNECTED`; completed enrollment
and credentials survive restart without enrollment traffic. This supersedes the
request-only startup behavior described in the earlier implementation phase.
See [completed enrollment handshake](../docs/node-enrollment-handshake.md)
for protocol, legacy keyless-approval safety, storage and two-node verification.
Operator identity, mesh sessions and a Core admin UI remain outside this phase.

## Federation heartbeats

Approved Agents now send signed outbound heartbeats, and Core persists reported
version/state plus server last-seen. Core outages drive persisted degraded/offline
states; bounded backoff and automatic reconnect preserve enrollment/credentials.
See [federation heartbeats](../docs/federation-heartbeats.md) for protocol, scheduling, admin detail,
privacy boundaries and `scripts/check-heartbeat-stack.py` live verification.
Heartbeats also report the locally persisted, default-off Peer Serving setting;
see [Opt-In Peer Serving](../docs/peer-serving.md). No operator identity or Core
admin UI is added.

## Membership invitations

`POST /api/v1/invitations` with exactly `{"role","expires_at","max_uses"}` asks
Core, signed as this Node, for a user-less membership invitation. The returned
secret is shown once and never stored by the Agent. See
[node invitations](../docs/node-invitations.md).

## Global member login

Global members sign in at ArkNet Core, never on the Node: the member listener's
`POST /api/v1/global-users/login` sends the browser to Core's `/node-login`
page, and `POST /api/v1/global-users/login/complete` verifies Core's one-time,
Node-bound assertion with Core's published key set and creates a session. The
Node never receives the global password and stores no password verifier. While
Core is unreachable, a browser that signed in online before can sign in again
until the Core-signed `offline_until` (at most 30 days). Enable it with
`ARKNET_GLOBAL_LOGIN_CALLBACK_URLS` (the member UI's `/login/callback` URLs)
and, if browsers reach Core at another address, `ARKNET_CORE_LOGIN_URL`. See
[global member login on a Node](../docs/cached-global-authorization.md).

## Release manifest

`ARKNET_RELEASE_MANIFEST` (Compose: `/etc/arknet/release.json`, mounted
read-only from `deploy/node/release.json`) names the current whole-stack
release manifest. The Agent parses it at startup and reports
`release: {"version","channel"}` in `GET /api/v1/status` only when the manifest
is valid and pins this Agent build; otherwise `release` is `null` and the
Agent keeps running. See [release manifest](../docs/release-manifest.md).

## Diagnostics

`GET /api/v1/diagnostics` reports Agent, PostgreSQL, Jellyfin, Core (live probe
with classified failure), private tracker, Tailscale, storage, ArkNet media
usage, transfers and version. Read-only, bounded (4 s probes, 2 s database), no
credentials sent. The UI's Diagnostics page explains each result. See
[Node diagnostics](../docs/node-diagnostics.md).

## Update discovery

The Agent checks `ARKNET_RELEASE_INDEX_URL` at startup and every 6 hours (or on
`POST /api/v1/updates/check`) for a newer, digest-verified release on its own
channel and reports it at `GET /api/v1/updates`. It never installs anything.
Automatic updates (`/api/v1/settings/automatic-updates`) default ON and the
operator's opt-out persists in PostgreSQL. See
[update discovery](../docs/update-discovery.md).

## Media-miss events

The Agent syncs this Node's media misses from Core outbound and applies the
local policy (`/api/v1/settings/media-miss-policy`, default `ASK`). ASK shows
operator notifications (`/api/v1/media-misses`) with Make Available Locally and
Ignore; AUTOMATIC queues and starts one missed object at a time within the
allocation. See [media-miss events](../docs/media-miss-events.md).

## Removed media

After each accepted heartbeat (and on media-list refresh) the Agent applies
explicit Core `REMOVED` tombstones: a verified `AVAILABLE` copy becomes
`REMOVED_RETAINED` and its file is kept; unverified selections become
`NOT_PRESENT`; in-flight transfers are cancelled. The operator may delete a
retained copy with `DELETE /api/v1/media/{media_id}/retained`, which targets
only `<ARKNET_MEDIA_DIR>/<media_id>.mp4`. See
[removed media](../docs/removed-media.md), including Jellyfin behavior.
