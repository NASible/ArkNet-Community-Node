# Core Development Compose Stack

Local Docker development environment for the ArkNet `0.0.1-dev` Core:

| Service | Purpose | Default host access |
| --- | --- | --- |
| `arknet-core` | Go Core API | http://127.0.0.1:8083 |
| `postgres` | PostgreSQL 18 development database | None; stack-only port 5432 |

Both services use the dedicated Compose `core` bridge network. Database data is
stored in the named `postgres-data` volume, normally
`arknet-core-dev_postgres-data`. The project name, network, and volume are
separate from the Node development stack.

## Prerequisites and local configuration

Use Docker Engine and Docker Compose v2+ with support for `--wait`. Host Go or
PostgreSQL installations are not required; Docker builds Core using a multi-stage
Dockerfile and runs it as a non-root user.

From the repository root:

```sh
cd deploy/core
cp .env.example .env
chmod 600 .env
openssl rand -hex 32
```

Using your editor, set `POSTGRES_PASSWORD` in the local `.env` to the generated
value. Do not overwrite an already configured `.env`. The example contains no
password, `.env` is ignored by Git, and Compose rejects an empty or unset
password. Use a separate password from your Node database. Never commit secrets.

Configuration:

| Variable | Default | Meaning |
| --- | --- | --- |
| `POSTGRES_PASSWORD` | Required | Unique local database password. |
| `POSTGRES_USER` | `arknet` | Initial development database user. |
| `POSTGRES_DB` | `arknet_core` | Initial development database name. |
| `ARKNET_CORE_PORT` | `8083` | Host loopback port mapped to container port 8080. |
| `ARKNET_CORE_LOG_LEVEL` | `INFO` | Core structured logging level. |
| `PGSSLMODE` | `disable` | Local private Docker development network only; remote databases should use verified TLS. |

Port 8083 avoids the standalone Core port 8080 and Node development ports. Change
it in `.env` if needed. PostgreSQL is not published to the host; do not expose it
as a workaround for connectivity issues.

## Launch and verify

From `deploy/core/`:

```sh
docker compose up --build -d --wait
docker compose ps
curl --fail http://127.0.0.1:8083/health
curl --fail http://127.0.0.1:8083/api/federation/v1/version
```

Expected responses:

```json
{"healthy":true,"database_healthy":true}
```

```json
{"version":"0.0.1-dev"}
```

For foreground logs, use `docker compose up --build`. Alternatively:

```sh
docker compose logs --tail 50 arknet-core postgres
```

PostgreSQL has a `pg_isready` health check. Core waits for that health check
before startup and has its own HTTP `/health` check. Core runs on `:8080` inside
the container, while the host publication stays loopback-only. Its shutdown
timeout is 10 seconds; Compose allows 15 seconds for graceful termination.

Core consumes `PGHOST=postgres`, `PGPORT=5432`, `PGDATABASE`, `PGUSER`,
`PGPASSWORD`, and `PGSSLMODE`. It connects and applies the embedded initial
`nodes` migration automatically before serving HTTP. The migration ledger and
schema changes are transactional and replay safely on restart. Existing database
users need permission to create the managed tables.

`/health` now includes a bounded live database ping. If PostgreSQL becomes
unavailable, Core returns HTTP 503 with
`{"healthy":false,"database_healthy":false}`. The running service recovers when
PostgreSQL returns without requiring a Core restart. Connection/migration failure
at initial startup exits nonzero rather than serving an uninitialized API.

Core's standalone database configuration defaults to TLS; this local stack
explicitly disables TLS only on its private development network. Do not use that
setting for remote deployments. See [Core schema and migration documentation](../../core/README.md)
for the internal repository, node fields, opaque IDs, and migration workflow.

Database inspection stays inside the stack:

```sh
docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
```

## Persistence, restart, and shutdown

```sh
# Restart both containers, retaining database data.
docker compose restart

# Stop and remove containers/network, retaining the named volume.
docker compose down

# Recreate containers using the retained data.
docker compose up -d --wait

# Rebuild after Core source changes; no source bind mounts are used.
docker compose up --build -d --wait
```

Do not use `docker compose down --volumes` for normal shutdown: it deletes the
database. Keep the same project name to reuse the same named volume.

The PostgreSQL 18 volume is mounted at `/var/lib/postgresql` rather than the
older `/var/lib/postgresql/data` path. Initialization variables apply only to an
empty data volume; changing the user, database, or password in `.env` does not
update an existing database. Preserve the original credentials or change them
explicitly within PostgreSQL.

## Automated checks

From the repository root (Python 3 and Docker Compose required):

```sh
python3 scripts/check-core-compose.py
python3 scripts/check-core-stack.py
```

The first check validates service scope, private database ports, volume layout,
health checks, startup ordering, and mandatory password configuration without
starting containers.

The integration check builds and starts a uniquely named disposable stack using
an available loopback port and a password generated only in its process
environment. It checks Core health/version JSON, database isolation, authenticated
database access from Core's network namespace, automatic migration, real node
create/read and migration tests, HTTP 503 during a database outage, automatic
health recovery, and a test row surviving both restart and down/up recreation. It removes only its own test containers, network,
database volume, and disposable Go test image; it never uses your existing development project.

## Optional private tracker overlay

The tracker is deliberately separate from the Core API and PostgreSQL. After
provisioning a dedicated Ed25519 signing/verification keypair, set:

```dotenv
ARKNET_TRACKER_ANNOUNCE_URL=https://tracker.example.net
ARKNET_TRACKER_SIGNING_KEY_FILE=/host/secret/path/tracker-private.key
ARKNET_TRACKER_VERIFY_KEY_FILE=/host/config/path/tracker-public.key
ARKNET_TRACKER_PORT=6969
```

The private-key file is mounted only into Core; the public-key file is mounted
only into `arknet-tracker`. Both files contain canonical padded base64 with no
trailing newline. The private file is 64 decoded bytes and the public file is 32.
Ensure UID 10001 can read the private file and the tracker's non-root UID can
read the public file without making the private key generally readable.

Start the explicit overlay with:

```sh
docker compose -f compose.yaml -f compose.tracker.yaml up --build -d --wait
docker compose -f compose.yaml -f compose.tracker.yaml ps
curl --fail http://127.0.0.1:${ARKNET_TRACKER_PORT:-6969}/health
```

The host publication is loopback-only for development. Production must put the
tracker behind an HTTPS endpoint that does not log credential-bearing announce
paths or query strings. It no longer uses or stores peer source addresses
(Prompt 48C): `ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE` must name a host file
containing the operator relays’ endpoint-signing public keys, one padded-base64
key per line. Only signed virtual relay endpoints are advertised. The tracker is attached only to its
own `tracker-edge` network; it receives no database password or Core credential.
See [private tracker design and grant format](../../docs/private-tracker.md).

## Optional private relay overlay

The relay (Prompt 48A, [relay README](../../relay/README.md)) splices
Node-to-Node streams so Nodes never learn each other's addresses. It verifies
Core's tracker grants with the **tracker verification key** and accepts
session revocations signed with a separate **relay-revocation key** (Core
signs them with that relay's own key, configured in `ARKNET_RELAYS_FILE`; see
[Operator relays](../../core/README.md#operator-relays)). Set:

```dotenv
ARKNET_RELAY_ID=relay-dc-1
ARKNET_RELAY_GRANT_VERIFY_KEY_FILE=/host/config/path/tracker-public.key
ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE=/host/config/path/relay-revocation-public.key
ARKNET_RELAY_TLS_CERT_FILE=/host/secret/path/relay-cert.pem
ARKNET_RELAY_TLS_KEY_FILE=/host/secret/path/relay-key.pem
```

The key files use the tracker-key format; the relay refuses to start if both
are the same key. The overlay also gives Core its side of the configuration:
set `ARKNET_CORE_RELAY_CONFIG_DIR` to a host directory (readable by UID 10001)
containing `relays.json`, `relay-list.key` and each relay's revocation
**private** key; Core mounts it read-only at `/run/config/arknet-relays`, so
`revocation_key_file` entries name paths there. Each relay's
`ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE` is the public half of its own key.
See [Operator relays](../../core/README.md#operator-relays). UID 65534 must be able to read all four files. Then:

```sh
docker compose -f compose.yaml -f compose.relay.yaml up --build -d --wait
```

The relay port (7443, TLS 1.3) is published on loopback unless
`ARKNET_RELAY_PUBLISH_ADDR` names a public address. The control port (7080:
health and revocations) is always loopback-only. The relay is attached only to
its own `relay-edge` network and receives no database password or Core secret.

## Optional Core-bound Node login

Global members sign in to Nodes through Core's `/node-login` page. Core signs a
one-time assertion with a dedicated Ed25519 key, which must not be the tracker
key. Provision the key, then set:

```dotenv
ARKNET_ASSERTION_SIGNING_KEY_FILE=/host/secret/path/assertion-private.key
# The next key before a rotation, retired keys afterwards. May be empty.
ARKNET_ASSERTION_PUBLISHED_KEYS_FILE=/host/config/path/assertion-published.keys
```

The private file uses the tracker-key format. Make it readable by UID 10001 and
no one else. Start the overlay with:

```sh
docker compose -f compose.yaml -f compose.node-login.yaml up --build -d --wait
curl --fail http://127.0.0.1:${ARKNET_CORE_PORT:-8083}/api/federation/v1/assertion-keys
```

To rotate the key:

1. Add the new public key to the published-keys file and restart. Then wait
   at least one hour (the key-set cache lifetime) so Nodes fetch it.
2. Make the new key the signing key and move the old public key into the
   published-keys file. Restart.
3. Remove the old public key once the offline window (30 days by default) has
   passed.

Serve Core over HTTPS: the login page takes the global password. See
[global login assertions](../../docs/global-login-assertions.md).

## Optional Core HTTP web seed

The web seed is an explicit Core overlay and requires the tracker-signing
configuration above because it validates the same short-lived, swarm-scoped
authorization. Place only approved regular files named by opaque media ID in a
dedicated host directory, then set:

```dotenv
ARKNET_CORE_WEB_SEED_ORIGIN=https://core.example.net
ARKNET_CORE_WEB_SEED_SOURCE=/host/approved/arknet-web-seed
```

Start all three definitions together:

```sh
docker compose -f compose.yaml -f compose.tracker.yaml -f compose.web-seed.yaml up --build -d --wait
```

The content directory is mounted read-only at `/srv/arknet/web-seed`. Do not use
an entire media library, host root, operator home, or writable upload directory.
Production ingress must use HTTPS and omit credential-bearing web-seed paths
from access logs. See [Core HTTP web seed](../../docs/core-web-seed.md).

## Scope

The base Compose file remains a local development environment, not production
deployment hardening. The tracker and web seed are enabled only by explicit
overlays. The
Docker administrator controls container state and data. No Headscale, Jellyfin,
Kubernetes, or user identities are introduced. Core database
application logic includes node enrollment, media catalog state, and stateless
tracker-grant issuance; the tracker itself has no database.

## Pending Node enrollment

Core now accepts minimal `POST /api/federation/v1/enrollments` requests and
idempotently stores untrusted pending nodes. No approval or trust promotion is
implemented. See [outbound enrollment and testing](../../docs/node-enrollment.md).

## Core enrollment administration

See [Core enrollment administration](../../docs/core-enrollment-administration.md) for the separate `/api/admin/v1/` endpoints,
scoped administrator tokens, pending pagination, approval and persisted
rejection. The completed signed handshake lets the Agent learn approval and
persist `CONNECTED`; private credentials are generated locally, not issued by
Core. No Core admin UI is implemented.

## Completed enrollment handshake

The Agent now persists a unique Ed25519 credential, signs enrollment and its own
approval-status requests, and learns Core approval outbound. Approval completion
atomically records federation enrollment and `CONNECTED`; completed enrollment
and credentials survive restart without enrollment traffic. This supersedes the
request-only startup behavior described in the earlier implementation phase.
See [completed enrollment handshake](../../docs/node-enrollment-handshake.md)
for protocol, legacy keyless-approval safety, storage and two-node verification.
Operator identity, mesh sessions and a Core admin UI remain outside this phase.

## Federation heartbeats

Approved Agents now send signed outbound heartbeats, and Core persists reported
version/state plus server last-seen. Core outages drive persisted degraded/offline
states; bounded backoff and automatic reconnect preserve enrollment/credentials.
See [federation heartbeats](../../docs/federation-heartbeats.md) for protocol, scheduling, admin detail,
privacy boundaries and `scripts/check-heartbeat-stack.py` live verification.
No operator identity or Core admin UI is added.
