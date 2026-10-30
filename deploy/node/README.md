# Node Development Compose Stack

Local development environment for the ArkNet `0.0.1-dev` community-node stack:

| Service | Purpose | Default host access |
| --- | --- | --- |
|| `arknet-ui` | React/TypeScript dashboard served by Vite | http://127.0.0.1:5174 |
|| `arknet-agent` | Local Go management API | http://127.0.0.1:8082 |
|| `postgres` | PostgreSQL 18 development database | None; stack-only port 5432 |
|| `jellyfin` | Jellyfin media server | http://127.0.0.1:8096 |

All services use the dedicated Compose `node` bridge network. Database data is
stored in the named `postgres-data` volume. The default project name is
`arknet-node-dev`, so the volume is normally `arknet-node-dev_postgres-data`.
PostgreSQL 18 requires the volume mount at `/var/lib/postgresql`, rather than
the older `/var/lib/postgresql/data` mount.

## Release manifest

[`release.json`](release.json) is the whole-stack release this directory
describes: release version and channel, every component image, and the SHA-256
of `compose.yaml` and `compose.transfer.yaml`. Component images are fixed in
Compose and cannot be overridden from `.env`. After changing either Compose
file, update its digest (`shasum -a 256 compose.yaml compose.transfer.yaml`);
the Agent test suite fails otherwise. See
[release manifest](../../docs/release-manifest.md).

## Installing a Node (operators)

Operators do not use this directory directly: [`install.sh`](install.sh)
installs a published release on a fresh Ubuntu LTS machine with no clone or
Compose editing. See [Node installation](../../docs/node-installation.md).
Pre-flight tests: `python3 -m unittest install_test` (Docker); fresh-VM
acceptance: `scripts/check-node-install-vm.py` (Lima).

## Upgrading an installed Node

Installed Nodes upgrade with [`arknet-node`](arknet-node) (`upgrade`,
`rollback`, `status`): it verifies the published release, pulls images, backs
up the database, applies the new Compose release without touching volumes,
validates the Agent and Jellyfin and rolls back automatically on failure. See
[Node upgrade](../../docs/node-upgrade.md). Unit tests: `python3 -m unittest
arknet_node_test`; live check: `scripts/check-node-upgrade-stack.py`.

## Prerequisites and local configuration

Use Docker Engine with Docker Compose v2+ (including support for `--wait`).
Community Nodes target Ubuntu LTS; Docker on macOS also works for development.
Go, Node.js, and npm do not need to be installed on the host for this stack.

From the repository root:

```sh
cd deploy/node
cp .env.example .env
chmod 600 .env
openssl rand -hex 32
```

Set `POSTGRES_PASSWORD` in the local `.env` to the generated value using your
editor. The example deliberately contains no password, and Compose refuses to
start with an empty password. `.env` is ignored by Git; never commit credentials.
Do not repeat the copy command over an existing configured `.env`.

The local configuration supports:

- `POSTGRES_PASSWORD`: required; unique local password.
- `POSTGRES_USER`: defaults to `arknet`.
- `POSTGRES_DB`: defaults to `arknet_node`.
- `ARKNET_UI_PORT`: defaults to `5174` on loopback.
- `ARKNET_UI_HTTPS_BIND`: defaults to `0.0.0.0:8443` (LAN management HTTPS).
- `ARKNET_TLS_DIR`: directory containing `cert.pem` and `key.pem`, mounted
  read-only into the UI; defaults to `./tls` for direct Compose development.
- `ARKNET_AGENT_PORT`: defaults to `8082` on loopback.
- `ARKNET_AGENT_LOG_LEVEL`: defaults to `INFO`.
- `PGSSLMODE`: defaults to `disable` only for the private local Docker network.
- `ARKNET_CORE_URL`: empty by default; opt in with an HTTPS Core base URL.
- `ARKNET_CORE_ALLOW_HTTP`: false by default; enable only for private HTTP development.

The host ports deliberately differ from standalone development (`5173` and
`8081`) so both workflows can run without stopping each other. Containers still
use ports `5173` and `8081`. If a host port is already occupied, change it in
`.env`. Do not publish PostgreSQL as a workaround for connectivity issues.

## Start and verify

From `deploy/node/`:

```sh
./arknet-node --home "$PWD" tls show   # generate a development certificate, print fingerprint

docker compose up --build -d --wait
docker compose exec -T arknet-agent arknet-agent operator setup-code
docker compose ps
```

For foreground development, `docker compose up --build` also starts all three
services and streams logs.

Open **https://<node-address>:8443/**, compare the printed fingerprint with the
browser's certificate details, and choose **Set up operator** using the printed
code. Subsequent visits require operator sign-in (password plus authenticator
or recovery code); use **Sign out** to revoke the session. The UI proxies `/api`
internally to `http://arknet-agent:8081`, not to the host's standalone Agent.
Plain HTTP at **http://127.0.0.1:5174/** remains available only on loopback.

```sh
docker compose exec -T arknet-agent arknet-agent status --json
curl --fail http://127.0.0.1:8082/health
docker compose logs --tail 50 arknet-agent arknet-ui postgres
```

Anonymous calls to `/api/v1/status` now return 401. Shell-based checks use the
local Agent CLI instead of bypassing HTTP authorization.

Status also contains `node_id` and `state_changed_at`, persisted across restart.
See the [state machine](../../docs/node-state-machine.md) for supported transitions.

Expected initial status is version `0.0.1-dev`, node state `PROVISIONING`, and Core
connectivity `NOT_IMPLEMENTED` when enrollment is disabled. With a configured
Core URL and successful acknowledgment, a fresh Node reports `ENROLLMENT_PENDING`.
Enrollment UI reporting remains a placeholder. Core is optional unless you opt in
to request submission; see [outbound enrollment](../../docs/node-enrollment.md).

PostgreSQL becomes healthy before the Agent starts; the Agent becomes healthy
before the UI starts. The Agent consumes the supplied `PG*` variables, migrates
and initializes its local identity, configuration, node state, and Core enrollment
tables automatically. Existing state is not overwritten during startup.

`/health` returns `{"healthy":true,"database_healthy":true}` when PostgreSQL is
reachable, or HTTP 503 with both fields false during an outage. Status reads
persisted identity/state and also returns 503 if the database cannot be read.
The local database contains no Core administrator secrets. See the
[Agent state documentation](../../node-agent/README.md) for details.

Inspect the database only from within the stack, for example:

```sh
docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
```

PostgreSQL has no published host ports. Its Docker `5432/tcp` metadata is not a
host publication. The bridge network is dedicated to this stack; outbound access
is not blocked, preserving the intended outbound-first Node architecture.
Docker administrators control the host and can still access container data;
this is a development environment, not a production security boundary.

## Shutdown, restart, and rebuild

```sh
# Restart containers, retaining data.
docker compose restart

# Stop and remove containers/network, retaining the named database volume.
docker compose down

# Recreate containers using the retained database.
docker compose up -d --wait

# Rebuild after Agent or UI source changes (no source bind mounts).
docker compose up --build -d --wait
```

Do **not** use `docker compose down --volumes` for normal shutdown: it destroys
the database volume. Changing `POSTGRES_USER`, `POSTGRES_DB`, or
`POSTGRES_PASSWORD` in `.env` does not update an already initialized database.
Retain the original credentials or change them deliberately inside PostgreSQL.
Keep the same project name to reuse the same volume.

The Agent runs as a non-root user and receives SIGTERM directly. Compose allows
15 seconds for shutdown, longer than the Agent's 10-second drain timeout. The UI
also runs as a non-root user. Component-scoped `.dockerignore` files exclude
local secrets, dependencies, and build outputs.

## Automated checks

From the repository root:

```sh
# Validate configuration without starting containers; Python 3 required.
python3 scripts/check-node-compose.py

# Build and exercise an isolated disposable test stack.
python3 scripts/check-node-stack.py
```

The integration check generates a password only in its process environment and
uses a unique test project with available loopback ports. It checks service
health, UI-to-Agent status, absence of PostgreSQL host ports, authenticated
PostgreSQL access from the Agent's network namespace, and persistence after
both restart and down/up recreation. It also checks real local-state repository
tests, Agent-only restart/recreation identity preservation, and database outage
and recovery without restarting Agent. It removes **only its disposable test
project, test volume, and helper test image**, not your development stack or database.

This stack uses Vite's development server, not a production UI server. Jellyfin,
mesh connectivity, Kubernetes, and production deployment hardening remain
outside this task.

## Optional mock transfer service

The normal stack remains unchanged. To test the [transfer interface](../../docs/transfer-service-interface.md)
and synthetic UI progress, explicitly add the development mock overlay:

```sh
export ARKNET_TRANSFER_TOKEN="$(openssl rand -hex 32)"
docker compose -f compose.yaml -f compose.transfer-mock.yaml up --build -d --wait
```

Keep the same overlay files for future stop/recreate commands. The transfer
container is isolated on an internal Agent-only control network, has no host
ports, Core/DB credentials, or media mounts. Select media, then choose **Start
mock transfer** in the UI; nothing starts automatically. It is not BitTorrent,
and mock completion is not real media availability. Restarting the mock clears
its in-memory progress.
Do not publish or commit the local service token.

## ArkNet media selection

Set `ARKNET_MEDIA_STORAGE_ALLOCATION_GIB` in `.env` before selecting media.
Use a whole GiB count, `0` (the default) to disable new selections, or `-1` for
unlimited allocation. Do not also set the legacy `_BYTES` variable. Apply a changed budget
with `docker compose up -d --force-recreate arknet-agent --wait`. The UI **Media**
view queues selections in the persistent Node PostgreSQL database; it does not
start downloads. Keep the database volume to retain the queue across restarts.
See [Node media selection](../../docs/node-media-selection.md) for allocation
semantics, states, API boundaries, and the disposable live check.

## Verify identity across Agent recreation

From `deploy/node/`, compare `node_id` in status before and after:

```sh
curl --fail http://127.0.0.1:8082/api/v1/status
docker compose restart arknet-agent
docker compose up -d --wait
curl --fail http://127.0.0.1:8082/api/v1/status
docker compose up -d --no-deps --force-recreate arknet-agent --wait
curl --fail http://127.0.0.1:8082/api/v1/status
```

The `node_id` stays identical. Keep the same project/volume and do not delete
PostgreSQL storage. A new empty database intentionally creates a new identity.

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

## Jellyfin media server

The Node stack includes Jellyfin (`jellyfin/jellyfin:12.2`, pinned by digest in
[`release.json`](release.json)) as a dedicated
media-serving container. Jellyfin starts independently of ArkNet Core; it depends
only on network availability and Docker volume mounts.

### Storage layout

The Compose stack uses named volumes while preserving the conceptual ArkNet paths:

| Volume | Container mount | Access | Content |
| --- | --- | --- | --- |
| `appdata` | `/config` | read-write | Persistent Jellyfin configuration, database, plugins, logs, and metadata |
| `jellyfin-cache` | `/cache` | read-write | Regenerable transcode and image cache |
| `media` | `/srv/arknet/media` | read-only | ArkNet-managed media library |
| `transfers` | `/srv/arknet/transfers` | read-write | Downloads, incomplete transfers, and temporary files |

`appdata` survives container recreation and host restart. `media` is mounted
read-only so Jellyfin cannot mutate ArkNet-managed library objects. The separate
cache volume prevents transient data from filling the container layer.

### Permissions

- The official `jellyfin/jellyfin` image runs as root by default. Named volumes
  are initialized by Docker and are writable by the container. If these volumes
  are replaced with host bind mounts, explicitly set ownership and permissions
  for the configured container UID/GID; do not make media writable merely to
  work around an ownership error.
- `/srv/arknet/media` must be readable by Jellyfin but remains read-only in the
  container. `/config`, `/cache`, and `/srv/arknet/transfers` must be writable.
- Jellyfin and PostgreSQL are attached to different Docker networks. Jellyfin
  receives no PostgreSQL hostname, port, volume, connection string, or
  credentials. The Agent is the only service attached to both the private Node
  network and the media-services network, solely so it can poll Jellyfin health.
- Keep Docker's storage location restricted to host administrators and allow
  enough space for Jellyfin metadata and cache in addition to the media itself.

### Agent integration

The Agent's `/api/v1/status` endpoint now reports `jellyfin_healthy` as:

- `true` — Jellyfin's `/health` probe returned HTTP 200.
- `false` — The health probe returned a non-200 status or timed out.
- `null` — No `JELLYFIN_API_URL` configured; the Agent has not been asked to check.

A `null` value does not indicate failure — it means Jellyfin is not configured
in the deploy stack. When configured, the Dashboard displays the status as
"Healthy", "Unhealthy", or "Not reported" alongside a plain-language description.

### Configuration

Jellyfin listens on `0.0.0.0:8096` inside the container. Compose maps it to the
loopback-only address `127.0.0.1:8096` by default. Edit `.env` to change:

- `JELLYFIN_BIND`: complete host bind address (default
  `127.0.0.1:8096`). For example, use `127.0.0.1:18096` to change the local
  port. Deliberately using `0.0.0.0:8096` exposes Jellyfin on every host
  interface and should be protected by the Node's ingress/authentication layer.
- `TZ`: timezone (default `UTC`). Jellyfin uses this for scheduling and display.
- `JELLYFIN_API_URL`: the Docker-internal URL the Agent polls. The provided
  Compose stack sets it to `http://jellyfin:8096`; it is not exposed to browser
  code.

### Deployment

Jellyfin uses `restart: unless-stopped`, so Docker restarts it after daemon or
host restart unless an operator explicitly stopped it. Compose mirrors the
image's built-in `/health` probe using `curl`, which is available in the official
image. Jellyfin has no dependency on Core, PostgreSQL, the Agent, or the UI;
those services can be unavailable while Jellyfin continues serving local media.

## Jellyfin health on the dashboard

The operator dashboard now includes a "Jellyfin" section between "Local services"
and "Federation". A Node Operator can answer all required questions without the CLI:

- **Is my node running?** → "Local services" section shows "Running" when both
  Agent and database health checks pass.
- **Is Core reachable?** → "Federation" card shows connectivity state ("Connected",
  "Disconnected", etc.) with human-friendly labels.
- **Is my node enrolled?** → Detail panel shows enrollment state ("Approved",
  "Pending approval", "Rejected", "Not enrolled").
- **When did it last contact Core?** → "Last successful Core contact" row shows
  a formatted timestamp or "Never" if uncontacted.
- **Is Jellyfin healthy?** → "Jellyfin" section and "Jellyfin health" row report
  healthy/unhealthy/not-reported status with guidance text.
