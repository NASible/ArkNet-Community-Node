# ArkNet

ArkNet is a privacy-focused distributed self-hosting platform with a centrally
coordinated **Core** and independently owned community **Nodes**.

## Repository layout

| Directory | Purpose |
| --- | --- |
| `core/` | Go Core API: federation trust, global identity, node enrollment, media catalog, authorization, swarm coordination, routing, and releases. |
| `node-agent/` | Go community-node agent: local services, local-only users, cached access for authorized global users, offline functionality, and private swarm participation. |
| `node-ui/` | React + TypeScript node management UI. |
| `transfer/` | Local transfer API and transfer-engine service boundary. |
| `tracker/` | Dedicated authorization-gated private BitTorrent v2 tracker; no Core database or public swarms. |
| `deploy/core/` | Docker Compose development stack for Core and its private PostgreSQL database, plus opt-in tracker and read-only HTTP web-seed overlays. |
| `deploy/node/` | Docker Compose development stack for the Agent, UI, local Jellyfin, and private PostgreSQL. |
| `schemas/` | Shared, versioned API and data contracts. |
| `docs/` | Architecture, development, and operations documentation; the existing prompt pack lives in `docs/development/`. |
| `scripts/` | Repository development and verification utilities. |

Operators install a Node on Ubuntu LTS with the bootstrap installer; see
[Node installation](docs/node-installation.md).

`VERSION` records the whole-stack development release: **0.0.1-dev**.

## Architecture boundaries

- Community Nodes are independently controlled, untrusted infrastructure.
  Use zero trust; Nodes primarily initiate outbound connections and node-to-node
  access is denied by default.
- Node operators cannot browse the global user directory. Global users receive
  pairwise pseudonymous IDs on each Node. Local users never synchronize to Core.
- Core tracks only ArkNet-managed objects, not arbitrary Node files.
- Media uses BitTorrent v2 private swarms with public DHT, PEX, and local peer
  discovery disabled. Peer serving is opt-in; private relay is preferred by
  default, with a Core HTTP web seed fallback.
- PostgreSQL will store ArkNet platform state. Keep services modular without
  introducing unnecessary microservices.

## Development status

The initial [Core Go service](core/README.md) provides application health and
whole-stack version endpoints, PostgreSQL node storage, automatic migrations,
environment configuration, structured logging, and graceful shutdown. Test it
from `core/` with `go test ./...` (Go 1.25+). Standalone `go run .` requires database
configuration; the Core Compose stack provides the normal local workflow.

The [Node Agent](node-agent/README.md) exposes database-aware health and local
status endpoints with a persistent opaque identity and typed node state (initially
`PROVISIONING`). It requires local PostgreSQL but no Core service. Use the Node
Compose stack for development, or configure a database before standalone
`go run .`; test from `node-agent/` with `go test ./...` (Go 1.25+).
Opt-in [outbound enrollment](docs/node-enrollment.md) establishes a unique
Node-held credential, learns administrative approval, and persists completed
enrollment. Approved Nodes send signed [federation
heartbeats](docs/federation-heartbeats.md); status reports observed Core
connectivity while unconfigured federation remains `NOT_IMPLEMENTED`. The
[Node state machine](docs/node-state-machine.md) validates explicit transitions,
persists timestamped history, and exposes the last change time through status.
Heartbeat failures drive bounded `DEGRADED`/`OFFLINE` transitions and successful
retry [reconnects automatically](docs/reconnect-behavior.md), with persisted
state visible through structured Agent logs and the refreshing read-only UI.
Local applications are isolated behind a small [application adapter
interface](docs/application-adapters.md); Jellyfin is the first implementation,
without dynamic plugin loading or Docker-socket access.

The [functional Node dashboard](docs/node-dashboard.md) is a read-only React +
TypeScript operator view that separates local Agent/database health from
federation enrollment/connectivity and last successful Core contact. Start it
from `node-ui/` with `npm ci` and `npm run dev`; test with `npm test` and build
with `npm run build`. It uses actionable local-service errors rather than
presenting an unexplained proxy status code.

The [Node development Compose stack](deploy/node/README.md) starts the Agent,
UI, Jellyfin, and private PostgreSQL with isolated database/media networks and
persistent named volumes. Configure `deploy/node/.env` from its password-free
example, then run `docker compose up --build -d --wait` from `deploy/node/`.
The dashboard is available at http://127.0.0.1:5174/ and Jellyfin at
http://127.0.0.1:8096/ by default.

The shared-schema directory remains a documented placeholder. The transfer
service has an explicit [local API boundary](docs/transfer-service-interface.md).
Core can issue opaque Node/swarm/action/expiry-scoped
[swarm authorization tokens](docs/swarm-authorization-tokens.md) to the separate
[private v2 tracker](docs/private-tracker.md); the tracker has no Core database,
user-profile, username, or watch-history access and exposes no public swarm.
The same short-lived download authorization gates Core's
[range-capable HTTP web seed](docs/core-web-seed.md), which maps only opaque
ArkNet media IDs to operator-approved files under a dedicated content root.
Core's database schema is embedded in `core/internal/postgres/migrations/`. Operator
and user identity are not implemented; scoped Core administrator tokens and
machine-held Node federation credentials remain separate security boundaries.

The [Core development Compose stack](deploy/core/README.md) starts Core and a
separate private PostgreSQL database with automatic migrations, database-aware
health checks, and persistent storage.
Configure `deploy/core/.env` from its password-free example, then run
`docker compose up --build -d --wait` from `deploy/core/`. Core health is available
at http://127.0.0.1:8083/health.

Community Nodes target Ubuntu LTS, Docker Engine, and Docker Compose. Core also
starts with Docker. Deployment definitions belong under `deploy/core/` and
`deploy/node/`; there are no Kubernetes manifests.

Verify the scaffold from the repository root using a POSIX shell (no language
runtime or package installation required):

```sh
sh scripts/check-scaffold.sh
```

When adding future components, inspect existing code, make the smallest change
for the current task, add meaningful tests, and update the relevant docs.
Never commit credentials, local media, or runtime state.

Node Agent local state is persisted in PostgreSQL; see [Agent storage and startup](node-agent/README.md). Continuous federation connectivity and user identities remain unimplemented. Core enrollment decisions are available through the administrative API with scoped, audited tokens.

## Mock transfer interface

Agent owns authorization; the optional local transfer service simulates byte
progress, pause/resume/cancel, completion, and verification failure without Core
credentials or real media writes. The Media UI displays mock progress separately
from local availability. See [transfer service interface](docs/transfer-service-interface.md)
and `scripts/check-transfer-stack.py`.

## Node media selection

The UI Media view displays authorized ArkNet objects and queues operator
selections locally. An explicit storage allocation bounds durable reservations;
the queue survives restart and remains visible offline. No automatic caching or
download worker is implemented. See [Node media selection](docs/node-media-selection.md).

## Core enrollment administration

See [Core enrollment administration](docs/core-enrollment-administration.md) for the separate `/api/admin/v1/` endpoints,
pending pagination, approval and persisted
rejection. The completed signed handshake lets the Agent learn approval and
persist `CONNECTED`; private credentials are generated locally, not issued by
Core. No Core admin UI is implemented. Administrators use scoped, audited tokens
minted on the Core host; see [Core administration](docs/core-administration.md).

## Completed enrollment handshake

The Agent now persists a unique Ed25519 credential, signs enrollment and its own
approval-status requests, and learns Core approval outbound. Approval completion
atomically records federation enrollment and `CONNECTED`; completed enrollment
and credentials survive restart without enrollment traffic. This supersedes the
request-only startup behavior described in the earlier implementation phase.
See [completed enrollment handshake](docs/node-enrollment-handshake.md)
for protocol, legacy keyless-approval safety, storage and two-node verification.
Operator identity, mesh sessions and a Core admin UI remain outside this phase.

## Federation heartbeats

Approved Agents now send signed outbound heartbeats, and Core persists reported
version/state plus server last-seen. Core outages drive persisted degraded/offline
states; bounded backoff and automatic reconnect preserve enrollment/credentials.
See [federation heartbeats](docs/federation-heartbeats.md) for protocol, scheduling, admin detail,
privacy boundaries and `scripts/check-heartbeat-stack.py` live verification.
No operator identity or Core admin UI is added.
