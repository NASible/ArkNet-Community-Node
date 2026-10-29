# Scripts

Small repository development and verification utilities belong here.

Docker stack checks bind-mount generated files, so keep them in the repository
checkout (git-ignored `/.arknet-*` directories, removed on exit). On macOS,
Docker Desktop may not see `/tmp` or `TMPDIR` (`/var/folders/...`). A file
mount from those locations silently becomes an empty directory. The MVP E2E and
Node upgrade checks probe their bind sources first and fail fast with that
explanation.

- `scripts/check-all.sh [--e2e] [--vm] [--logs DIR]`: the one-command gate.
  Without flags it runs the component checks: scaffold, security hygiene,
  gofmt, `go test`/`go vet` for every Go module, `node-ui` tests and build,
  every `scripts/*_test.py` and `deploy/node/*_test.py`, the Core and Agent
  PostgreSQL store tests against a throwaway `postgres:18-alpine` (run from a Go
  container that shares the database's clock), and worker tests inside the
  transfer image (real libtorrent). `--e2e` adds the MVP E2E, Node upgrade and product swarm
  checks; `--vm` adds the fresh-VM install check. Every step runs, step logs
  stay in `DIR`, and the last line is `EXIT <failed steps>`.
- `python3 scripts/check-mvp-e2e.py [--setup-only] [--results FILE] [--diagnostics-dir DIR]`:
  Core, Node A and Node B acceptance for Prompt 43 items 2–16, 19 and 20.
- **Failure diagnostics.** When `check-mvp-e2e.py` hits an exception *or any
  item FAILs*, or `check-multi-node-swarm-stack.py` fails, the harness writes a
  sanitized bundle before cleanup. The bundle contains per-Node status,
  transfers, media and seeds, worker events, service log tails, item results
  and the failure traceback. It goes to `--diagnostics-dir`, else next to
  `--results`, else the system temp dir. `scripts/e2e_diagnostics.py` redacts
  every known secret, grant-bearing URL segment, `Authorization` value and the
  work path, then re-scans the bundle. A bundle that still contains a known
  secret is deleted, not left on disk. `--inject-failure after-setup` forces a
  failure after setup, carrying a known secret, to exercise this path.
- `python3 scripts/check-node-upgrade-stack.py`: Agent update discovery
  (item 17) and whole-stack upgrade, failed-release rollback and manual
  rollback with data preserved (item 18). Agent discovery uses
  `host.docker.internal`, so this check needs Docker Desktop.

Run the foundation check from any working directory:

```sh
sh /path/to/ArkNet/scripts/check-scaffold.sh
```

The check uses only a POSIX shell and standard utilities. It verifies required
directories, directory documentation, the root README and ignore file, and the
initial `0.0.1-dev` version marker. It does not start or test services.

Node Compose checks (Python 3 and Docker Compose required):

```sh
python3 /path/to/ArkNet/scripts/check-node-compose.py
python3 /path/to/ArkNet/scripts/check-node-stack.py
```

`check-node-compose.py` validates the four-service contract without starting
containers, including Jellyfin storage, read-only media, network isolation, and
Agent health polling. `check-node-stack.py` builds and starts a uniquely named disposable
stack, validates HTTP status/proxying and database isolation/connectivity, and
proves storage survives both restart and down/up recreation. It generates a
password in its process environment only and removes only its own test project's
containers, networks, and volumes. It does not touch your development database.

The Jellyfin acceptance check is also disposable:

```sh
python3 /path/to/ArkNet/scripts/check-jellyfin-stack.py
```

It verifies healthy startup without Core, UI-reported health, persistent `/config`
across container recreation, `unless-stopped` restart policy, read-only media,
and network isolation from PostgreSQL. It removes only its unique test project.

See [Node deployment](../deploy/node/README.md) for normal startup, credentials,
and data-preserving shutdown commands.

Core Compose checks (Python 3 and Docker Compose required):

```sh
python3 /path/to/ArkNet/scripts/check-core-compose.py
python3 /path/to/ArkNet/scripts/check-core-stack.py
```

`check-core-compose.py` validates the two-service contract, private database,
health checks, startup ordering, and password configuration without starting
containers. `check-core-stack.py` builds and exercises a uniquely named
disposable Core/PostgreSQL stack, tests live HTTP endpoints and DB connectivity,
automatic migrations, real node create/read and migration behavior, database
outage/recovery, and persistence across restart and down/up recreation. It removes only its
own test project's containers, network, volume, and temporary Go test image. No local password file is
created and existing development stacks are not used.

See [Core deployment](../deploy/core/README.md) for normal startup and shutdown.

The Node live-stack check also verifies local-state initialization against real PostgreSQL, identity preservation across Agent-only restart/recreation, and HTTP 503 during database outage followed by recovery. Its disposable helper test image is removed during cleanup.

The Node live check now exercises a real explicit transition and verifies state
and timestamp preservation through restart/recreation, plus unchanged history on
invalid transitions. `node-state-probe.go` is test-only Go source mounted into
the disposable build container at `/src/probe/main.go`; it is not a host command
or public state-change endpoint.

Enrollment end-to-end check (Python 3 and Docker Compose required):

```sh
python3 scripts/check-enrollment-stack.py
```

Starts isolated Core and Node projects with separate private databases and a
shared application-only test transport network. Verifies real outbound enrollment,
concurrent idempotent retries, identifying-field rejection, restart/recreation,
and real repository tests. It removes only its disposable resources. No local
`.env` or development database is modified. The ordinary Node stack check
explicitly disables enrollment so it stays independent of Core.

Run live stack checks sequentially on constrained Docker VMs. Go helper containers
limit compilation parallelism and match the static application build flags to
reuse compiled packages; these settings do not change application containers.

## Core enrollment administration live check

```sh
python3 scripts/check-core-admin-stack.py
```

Uses an isolated Core/PostgreSQL 18 project and generated development credentials
to verify authorization, complete pagination, approval/rejection, invalid and
duplicate actions, concurrent decisions, restart/recreation persistence and real
repository tests. It never modifies a developer `.env` or existing services.

## Private tracker verification

```sh
python3 scripts/check-private-tracker-stack.py
```

Builds a uniquely named standalone tracker image, generates a disposable
dedicated Ed25519 keypair and Core-format swarm tokens, and proves that a bare
endpoint plus v2 hash and an expired token are denied while two authorized peers
can join and receive compact peer data. It also proves download-only and
seed-only action enforcement, stopped-peer removal, absence of Core/DB
credentials, public-key-only mounting, and that logs omit grants, hashes, and
peer IDs. Only its own container and image are removed.

## Multi-node private swarm proof

```sh
python3 scripts/check-multi-node-swarm-stack.py --results /path/to/result.json
```

Core plus two isolated Nodes. Node A replicates a test object from the metered
Core web seed (the Core-only baseline) and opts in to Peer Serving. Node B then
downloads from a Node A test adapter and Core, and Node A is stopped
mid-transfer. Results are accepted only when byte accounting proves that no
unmeasured source served Node B. Takes about 10 minutes. See
[multi-node swarm proof](../docs/multi-node-swarm-proof.md).

## Mock transfer verification

```sh
python3 scripts/check-transfer-stack.py
```

Reuses the disposable media-selection check with the explicit transfer-mock
overlay. Exercises Agent authorization, the real local HTTP boundary, fake
progress, pause/resume/cancel, completion and verification-failure reports, and
sidecar network/credential isolation. No BitTorrent or actual media is downloaded.
Only uniquely named test projects are cleaned up.

## Media selection verification

```sh
python3 scripts/check-media-selection-stack.py
```

Uses disposable Core/Node projects to verify real signed catalog fetching, local
queue persistence across Agent restart/recreation, idempotency, storage allocation
rejection, explicit removal tombstones, and offline retention. It does not start
a UI or Jellyfin and removes only its own test stacks. See
[Node media selection](../docs/node-media-selection.md).

## Two-node enrollment handshake verification

```sh
python3 scripts/check-node-handshake-stack.py
```

Creates isolated Core and two Node PostgreSQL stacks (no UI) to verify unique
pinned keys, node-bound authentication, A-as-B rejection, outbound approval
completion and restart/recreation with Core offline. Runs real PostgreSQL suites
and removes only its own resources. `node-enrollment-auth-probe.go` is an internal
integration helper, not a production credential export or management endpoint.

## Federation heartbeats

Approved Agents now send signed outbound heartbeats, and Core persists reported
version/state plus server last-seen. Core outages drive persisted degraded/offline
states; bounded backoff and automatic reconnect preserve enrollment/credentials.
See [federation heartbeats](../docs/federation-heartbeats.md) for protocol, scheduling, admin detail,
privacy boundaries and `scripts/check-heartbeat-stack.py` live verification.
No operator identity or Core admin UI is added.

The same live check also exercises [automatic Core reconnect](../docs/reconnect-behavior.md)
from `MESH`, verifies offline Agent/database/UI availability and structured state
transition logs, then proves Core restart recovers without enrollment reset.
