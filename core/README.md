# ArkNet Core

Go Core HTTP service (Go 1.25+) with PostgreSQL-backed node storage. The only
direct external Go dependency is the pinned `pgx/v5` PostgreSQL driver.
Signed Node enrollment, approval, heartbeats, and approved-Node media catalog
reads are implemented. User identities are not. See the
[enrollment contract](../docs/node-enrollment.md) and
[media catalog](../docs/media-catalog.md).

## Recommended local development

Use the [Core development Compose stack](../deploy/core/README.md) to start Core
and its private PostgreSQL database together. After configuring `deploy/core/.env`:

```sh
cd deploy/core
docker compose up --build -d --wait
curl --fail http://127.0.0.1:8083/health
curl --fail http://127.0.0.1:8083/api/federation/v1/version
```

Healthy responses:

```json
{"healthy":true,"database_healthy":true}
```

```json
{"version":"0.0.1-dev"}
```

`/health` performs a live database ping with a 2-second timeout. Database failure
returns HTTP 503 and `{"healthy":false,"database_healthy":false}` without exposing
driver errors or credentials. Version reporting remains independent of database
health. Unsupported methods return 405; unknown routes return 404. Go's router
also supports HEAD on GET routes.

## Standalone configuration

To run `go run .` from `core/`, first configure a reachable PostgreSQL database.
The Compose database is intentionally not published to the host. Use Compose for
the normal local workflow rather than exposing that database just to run Core.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ARKNET_CORE_ADDR` | `127.0.0.1:8080` | HTTP listener in `host:port` form. |
| `ARKNET_CORE_LOG_LEVEL` | `INFO` | Standard `slog` level; case-insensitive and supports numeric offsets. |
| `ARKNET_CORE_SHUTDOWN_TIMEOUT` | `10s` | Positive Go duration for draining requests. |
| `ARKNET_TRACKER_ANNOUNCE_URL` | Unset | Private tracker origin (scheme, host, and optional port only). HTTPS is required except for loopback and the exact case-insensitive `arknet-tracker` sidecar hostname. Must be set together with the signing-key file. |
| `ARKNET_TRACKER_SIGNING_KEY_FILE` | Unset | File containing exactly the canonical base64 encoding of a 64-byte Ed25519 private key. Must be set together with the tracker origin. |
| `ARKNET_CORE_WEB_SEED_ORIGIN` | Unset | Public Core origin used to construct authorized web-seed URLs. Must be HTTPS except for loopback or the exact `arknet-core` development sidecar, and must be paired with the web-seed root. |
| `ARKNET_CORE_WEB_SEED_ROOT` | Unset | Absolute, clean, non-root directory containing files named exactly by opaque media ID. Must be paired with the web-seed origin and requires tracker signing configuration. |
| `ARKNET_ASSERTION_SIGNING_KEY_FILE` | Unset | Enables Core-bound Node login. Same format as the tracker key (a single trailing newline is allowed). It must be a dedicated key, not the tracker key. |
| `ARKNET_ASSERTION_PUBLISHED_KEYS_FILE` | Unset | Optional extra Ed25519 public keys to publish without signing: the next key before a rotation, retired keys afterwards. Canonical base64, one per line, `#` comments allowed, at most 8. |
| `ARKNET_ASSERTION_OFFLINE_WINDOW` | `720h` | Offline login window written into assertions; a Go duration from `1m` to `720h` (30 days, decision D3). Short windows are for tests. |
| `DATABASE_URL` | Unset | Full PostgreSQL connection URL; overrides the `PG*` variables below. |
| `PGHOST` | `127.0.0.1` | Database host. |
| `PGPORT` | `5432` | Database TCP port. |
| `PGDATABASE` | `arknet_core` | Database name. |
| `PGUSER` | `arknet` | Database user. |
| `PGPASSWORD` | Empty | Database password; use environment/local secrets, not source control. |
| `PGSSLMODE` | `require` | PostgreSQL TLS mode. Use `verify-full` with trusted certificates for remote deployments. |

The local Compose stack explicitly uses `PGSSLMODE=disable` on its dedicated
private development network. Do not copy that setting to a remote deployment.
Passwords supplied through `PGPASSWORD` are safely URL-encoded; if using a full
`DATABASE_URL`, encode reserved characters yourself. Never print connection URLs
or commit credentials. Invalid connection configurations fail with sanitized
errors. Unset/empty variables use defaults.

Structured JSON logs go to stdout with startup address/version, database-ready,
shutdown, and errors. No connection strings, user identities, or request bodies
are logged. HTTP timeouts remain 5s for headers, 15s for reads, 30s for writes,
and 60s for idle connections.

## Nodes and repository boundary

`internal/nodes.Repository` defines `Create` and `GetByPublicID`; the PostgreSQL
implementation lives in `internal/postgres`. Queries are parameterized. The
separate `nodes.EnrollmentRepository` accepts a validated Agent-generated opaque
ID through `POST /api/federation/v1/enrollments`, idempotently inserting a pending
untrusted record. No administrator node creation, approval, or directory endpoint
is exposed. See [enrollment](../docs/node-enrollment.md) for payload and errors.

`nodes` stores:

| Column | Meaning |
| --- | --- |
| `id` | Internal generated bigint primary key; not the public identifier. |
| `public_id` | Unique `node_` prefix plus 64 lowercase hex characters from 256 cryptographically random bits. |
| `enrollment_status` | Typed initial state `PENDING`. |
| `trust_status` | Typed initial state `UNTRUSTED`. |
| `software_version` | Node-reported software version. |
| `created_at` | Database-generated timezone-aware creation timestamp. |
| `last_seen_at` | Nullable timezone-aware timestamp; initially unknown, not fabricated. |

Public ID generation accepts no operator names, hostnames, location, emails, or
other identifying inputs. Both the repository and database enforce the ID format;
the unique constraint rejects duplicates. Creating a record cannot promote its
trust or enrollment state. Status transitions and last-seen updates are future
work, not implemented here.

## Automatic migrations

Core connects with a bounded startup context, then applies embedded SQL migrations
before serving HTTP. Fresh databases require no separate migration command.
Startup fails if connection or migration fails; an already running Core reports
database outages through `/health` and reconnects after the database returns.

Migration SQL lives in `internal/postgres/migrations/`, beginning with
`0001_nodes.sql`. A PostgreSQL transaction-scoped advisory lock serializes
concurrent startups. Schema changes and ledger entries commit together in a
single transaction. The `arknet_schema_migrations` ledger records migration
version, filename, SHA-256 checksum, and application timestamp. Repeated startup
is idempotent; edited applied migrations are rejected.

Do not edit an applied migration or manually repair its ledger. Add a new
numbered forward migration for subsequent schema changes. Existing unrelated
objects are not scanned, synchronized, or modified. An unexpected pre-existing
`nodes` table without the ArkNet ledger causes startup to fail rather than
silently replacing it. Back up existing data before applying schema changes.

## Media catalog

`internal/media` defines ArkNet-managed media metadata and
`store.MediaCatalogRepository` defines creation, lookup, listing, and terminal
removal. PostgreSQL stores Core-generated opaque IDs, media type, title, size,
`ACTIVE`/`REMOVED` state, an optional BitTorrent v2 info-hash placeholder, and
creation/update timestamps. Omitting the state on repository creation defaults to
`ACTIVE`; unknown explicit states are rejected. It stores no Node filesystem path.

Approved, trusted Nodes use signed, node-scoped read endpoints under
`/api/federation/v1/nodes/{node_id}/media`. Lists include removed tombstones so a
Node can identify a previously seen object as removed. No unauthenticated global
catalog or federation mutation endpoint is exposed. See
[Core media catalog](../docs/media-catalog.md) for the contract and boundaries.

When both tracker variables are configured, an approved, trusted Node may send a
signed `POST` (with no query) to
`/api/federation/v1/nodes/{node_id}/media/{id}/tracker-grants`. Core issues a
five-minute, stateless Ed25519 grant only for an `ACTIVE` object with a nonempty,
canonical 64-character lowercase hexadecimal BitTorrent v2 info hash. The
empty signed body remains download-only so peer serving is not enabled
implicitly. A strict signed JSON body may explicitly request `download`, `seed`,
or both. The `no-store` JSON response contains exactly `tracker_url`,
`expires_at`, and canonical `actions`; it has no user, profile, watch-state, or
local-username fields. The grant's signed 58-byte binary payload is version byte
`2`, big-endian uint64 Unix expiry, a 16-byte pairwise Node/swarm subject, the
decoded 32-byte full v2 hash, and a nonzero action mask (`0x01` download, `0x02`
seed). The subject is the
first 16 bytes of SHA-256 over a domain-separated Ed25519 signature made with
Core's dedicated tracker signing key over the authenticated Node ID and full
v2 hash; it exposes no Node ID, differs across swarms, and cannot be dictionary
tested using the tracker's public key. The URL is formed as
`<configured-origin>/announce/<grant>`.

Rejected, blocked, pending, or otherwise untrusted Nodes receive no new token.
See [swarm authorization tokens](../docs/swarm-authorization-tokens.md) for the
strict request, token, action, expiry, and tracker-validation contract.

The configured tracker URL is an origin, not an announce path: userinfo, paths
(other than a normalized `/`), query strings, fragments, malformed ports,
encoded/Unicode host ambiguity, and IPv6 zones are rejected at startup. Plain
HTTP is accepted only for `localhost`, loopback IP literals, or the exact
case-insensitive `arknet-tracker` sidecar hostname. Leaving both tracker variables
unset disables issuance; setting only one is a startup error. Signing-key files
are size-bounded, strictly decoded, and checked against the public half derived
from their seed. Keep the file out of source control and logs.

When both web-seed variables and the dedicated tracker issuer/key are configured,
download-capable tracker-grant responses also include `web_seed_url`; seed-only
responses omit it. The URL reuses that response's signed grant and addresses exactly
`GET|HEAD /api/web-seed/v1/media/{id}/{grant}`. Only download-capable, unexpired
v2 grants for the `ACTIVE` object's full canonical v2 hash are accepted. Core
opens only the file named `<mediaID>` beneath the configured root with Go's
root-confined filesystem API; symlinks, directories, absent files, and size
mismatches are not served. Successful responses are non-cacheable binary content
and support RFC byte ranges through `http.ServeContent`. Leaving both web-seed
variables unset keeps the route disabled and preserves the three-field tracker
grant response.

## Build and shutdown

Within `core/`:

```sh
go build -o bin/arknet-core .
# Requires database environment as described above.
./bin/arknet-core
```

SIGTERM or SIGINT stops HTTP acceptance, drains requests, then closes the database
pool. Successful shutdown logs `shutdown_complete` and exits 0. Deadline failure
closes remaining connections and exits 1. Use the compiled process for signal
verification rather than signaling the `go run` wrapper.

## Tests

Within `core/`:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Unit tests cover HTTP health success/failure/cancellation, version reporting,
configuration, opaque ID format, signed media authorization and tombstones,
input validation, and server shutdown.

Real repository/migration tests require `ARKNET_TEST_DATABASE_URL`. When absent,
those tests explicitly skip; they do not substitute mocks. To run them against
isolated real PostgreSQL without configuring a host database, from repository root:

```sh
python3 scripts/check-core-compose.py
python3 scripts/check-core-stack.py
```

The integration runner builds a disposable Go test image and runs node and media
create/read, uniqueness, concurrent/idempotent migration, checksum drift, and rollback tests
against PostgreSQL. Each repository test owns a random temporary schema and
removes only that schema. The runner also verifies automatic startup migration,
HTTP 503 during database outage, recovery without restarting Core, and database
persistence. Its test project, volume, and temporary test image are removed;
your normal development database is not touched.

The enrollment end-to-end check is `python3 scripts/check-enrollment-stack.py`
from repository root. It starts fresh disposable Core and Node databases and
verifies one pending record despite concurrent retries and Agent recreation.

## Core enrollment administration

See [Core enrollment administration](../docs/core-enrollment-administration.md) for the separate `/api/admin/v1/` endpoints,
explicit development-only authorization, pending pagination, approval and persisted
rejection. The completed signed handshake lets the Agent learn approval and
persist `CONNECTED`; private credentials are generated locally, not issued by
Core. No Core admin UI is implemented.

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
No operator identity or Core admin UI is added.

## Global users

Core owns ArkNet global user accounts with opaque `user_…` IDs, argon2id
password hashes, `ACTIVE`/`DISABLED` status, and 12-hour bearer sessions stored
only as SHA-256 digests. In this MVP, administrators create users through
`/api/admin/v1/users`; there is no self-registration or external identity
provider. Login is `POST /api/identity/v1/sessions`. Disabling a user blocks new
sessions and revokes existing ones. See [global users](../docs/global-users.md).

## Node membership invitations

Approved Nodes create user-less invitations (role, expiry, max uses) through the
signed `POST /api/federation/v1/nodes/{node_id}/invitations`. Core stores only
the SHA-256 digest of the 256-bit secret and rejects replayed signed requests.
Authenticated global users accept with `POST /api/identity/v1/invitations/accept`,
which atomically creates the membership. Operators can't search or list global
users. See [node invitations](../docs/node-invitations.md).

## Pairwise membership IDs

Each membership has a random `member_…` ID (256 bits, not derived from the user
or Node), so the same user is unlinkable across Nodes. Core has no Node-facing
bulk membership route; a Node learns a pairwise ID only through a member's
authorization flow. See [pairwise membership IDs](../docs/pairwise-membership-ids.md).

## Cached global authorization

Core verifies online Node-member login using the existing global password hash
without exposing that hash, the username, or the Core user ID. Signed Node
responses contain only pairwise membership identity, allowed Node, explicit
permissions, validation time, and bounded expiration. Signed reconnect refresh
reports current authority for already cached pairwise IDs, including explicit
revocation for disabled or removed members. See
[cached global authorization](../docs/cached-global-authorization.md).

## Core-bound Node login

With `ARKNET_ASSERTION_SIGNING_KEY_FILE` set, Core serves a minimal login page
at `/node-login`. A Node sends the browser there with its `node_id`, a nonce, a
PKCE-style challenge and a callback it registered with
`PUT /api/federation/v1/nodes/{node_id}/login-callbacks` (signed). After the
user signs in, Core checks the membership, claims the nonce once, and
redirects to the callback with a one-time, Node-bound assertion in the URL
fragment. Nodes verify it with the key set at
`GET /api/federation/v1/assertion-keys`. The Node never sees the password. See
[global login assertions](../docs/global-login-assertions.md#core-implementation-46a).

## Primary home node

Each global user has at most one home node, chosen explicitly with
`PUT /api/identity/v1/home-node` from their own memberships and read with
`GET`. Core routing reads it through a read-only internal interface. Nodes never
receive home designations or other memberships. See
[primary home node](../docs/home-node.md).

## Media routing

`GET /api/routing/v1/media/{media_id}` (Core session) returns
`{"target":"HOME_NODE","node_id":…}` only when the caller's home node is
healthy (fresh accepted heartbeat, active state, healthy report) and holds a
verified replica of the active object; otherwise `{"target":"CORE"}`. See
[media routing](../docs/media-routing.md).

## Media-miss events

When routing falls back to Core because the home node lacks an active object,
Core records an aggregated (Node, object) miss off the request path (bounded,
never awaited, no user data). Nodes read their own misses with the signed
`GET /api/federation/v1/nodes/{node_id}/media-misses`. See
[media-miss events](../docs/media-miss-events.md).

## Removed media

Administrators mark an ArkNet object `REMOVED` with
`POST /api/admin/v1/media/{media_id}/remove`. The tombstone is terminal and
blocks new grants, web-seed/metadata serving, replica reports, and peer-source
selection. Core stores no Node paths or Jellyfin state. See
[removed media](../docs/removed-media.md).
