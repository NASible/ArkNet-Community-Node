# ArkNet Hermes Agent Prompt Pack

## How to Use This Pack

Give Hermes **one prompt at a time, in order**.

For every task:

- Inspect the existing repository before changing anything.
- Preserve working behavior from previous tasks.
- Do not implement future roadmap items unless required by the current task.
- Prefer simple, maintainable solutions over abstraction for its own sake.
- Add tests for meaningful behavior.
- Update relevant documentation.
- Do not introduce Kubernetes; community nodes use Docker Compose.
- Do not expose secrets in source control.
- Keep Core and Node code modular, but avoid premature microservices.
- Use opaque IDs rather than personally identifying values.
- Keep zero-trust and privacy requirements in mind throughout.

After each task, Hermes should report:

1. What changed.
2. Files created or modified.
3. How to run/test it.
4. Any assumptions made.
5. Any known limitations or follow-up items.

---

# PHASE 0 — Repository Foundation

## Prompt 01 — Create the ArkNet Monorepo Structure

You are building the initial ArkNet distributed platform repository.

Create a clean monorepo structure for:

- `core/` — Go ArkNet Core API
- `node-agent/` — Go community-node agent
- `node-ui/` — React + TypeScript node management UI
- `transfer/` — future BitTorrent v2 transfer service
- `deploy/core/`
- `deploy/node/`
- `schemas/`
- `docs/`
- `scripts/`

Requirements:

- Add a root README explaining each directory.
- Add sensible `.gitignore` rules.
- Add an initial version marker using `0.0.1-dev`.
- Do not implement application logic yet.
- Do not add Kubernetes manifests.
- Keep the repository ready for Docker Compose based development.

Acceptance criteria:

- Repository structure is clear and documented.
- No unnecessary frameworks or dependencies are introduced.
- A new developer can understand where future ArkNet components belong.

---

## Prompt 02 — Bootstrap the Core Go Service

Create the initial `arknet-core` Go service.

Requirements:

- Use idiomatic Go.
- Expose an HTTP server.
- Add:
  - `GET /health`
  - `GET /api/federation/v1/version`
- `/health` should return whether the application itself is healthy.
- `/api/federation/v1/version` should return `0.0.1-dev`.
- Add structured logging.
- Support configuration through environment variables.
- Add graceful shutdown.
- Add unit tests for the HTTP endpoints.
- Do not add authentication or PostgreSQL yet.

Acceptance criteria:

- `go test ./...` succeeds.
- Core starts locally.
- Both endpoints return valid JSON.
- SIGTERM causes a graceful shutdown.

---

## Prompt 03 — Bootstrap the Node Agent

Create the initial `arknet-agent` Go service.

Requirements:

- Expose a local HTTP management API.
- Add:
  - `GET /health`
  - `GET /api/v1/status`
- Status should currently contain:
  - ArkNet version
  - node state
  - Core connectivity placeholder
- Initial node state should be `PROVISIONING`.
- Use structured logging and graceful shutdown.
- Configuration should come from environment variables.
- Add tests.
- Do not implement Core communication yet.

Acceptance criteria:

- Agent starts independently.
- `/health` and `/api/v1/status` return valid JSON.
- Tests pass.
- State names are represented by constants or a typed enum-style implementation rather than arbitrary strings throughout the code.

---

## Prompt 04 — Bootstrap the ArkNet Node UI

Create the initial ArkNet Node management UI using React and TypeScript.

Requirements:

- Keep styling simple and functional.
- Create a dashboard showing:
  - ArkNet version
  - Node state
  - Core connectivity
  - Enrollment state
- Fetch data from the Node Agent `/api/v1/status`.
- Handle:
  - loading
  - agent unavailable
  - successful response
- Do not implement configuration controls yet.

Acceptance criteria:

- UI starts locally.
- UI reads status from the Node Agent rather than hardcoding values.
- Agent failure produces a useful operator-facing message.
- Code is structured so additional pages can later be added.

---

# PHASE 1 — Local Development Environment

## Prompt 05 — Create the Node Development Compose Stack

Create a Docker Compose development environment for an ArkNet community node.

Include:

- `arknet-agent`
- `arknet-ui`
- PostgreSQL

Requirements:

- Use named volumes for persistent database data.
- Use a dedicated Docker network.
- Do not expose PostgreSQL externally by default.
- Agent should be able to reach PostgreSQL.
- UI should be able to reach the Agent.
- Add `.env.example`.
- Do not store passwords in committed `.env` files.
- Add documented startup and shutdown commands.

Do not add Jellyfin yet.

Acceptance criteria:

- `docker compose up` starts all three services.
- UI displays Node Agent status.
- PostgreSQL remains private to the stack.
- Restarting containers preserves database storage.

---

## Prompt 06 — Create the Core Development Compose Stack

Create a development Docker Compose stack for ArkNet Core.

Include:

- `arknet-core`
- PostgreSQL

Requirements:

- PostgreSQL should not be exposed publicly by default.
- Use persistent volumes.
- Add health checks.
- Core should wait for the database to become healthy where appropriate.
- Add `.env.example`.
- Document how to launch the Core development environment.

Do not add Headscale, Jellyfin, tracker, or web seed yet.

Acceptance criteria:

- Core and PostgreSQL start together.
- Core health endpoint works.
- Database data survives container restart.

---

# PHASE 2 — Database Foundations

## Prompt 07 — Add PostgreSQL and Migrations to Core

Integrate PostgreSQL into `arknet-core`.

Create a migration system and initial schema containing:

`nodes`

Fields should include at minimum:

- internal primary key
- opaque public node ID
- enrollment status
- trust status
- software version
- created timestamp
- last-seen timestamp

Requirements:

- Public node IDs must not contain operator names, hostnames, geographic information, email addresses, or other identifying information.
- Generate IDs using a collision-resistant opaque identifier format.
- Add database connection health to `/health`.
- Add repository/data-access code behind a clean internal interface.
- Do not implement user identities yet.

Acceptance criteria:

- Fresh database migrates automatically or through a documented migration command.
- Tests cover creating and reading node records.
- Core reports unhealthy when the database is unavailable.

---

## Prompt 08 — Add PostgreSQL State to the Node Agent

Integrate PostgreSQL into `arknet-agent`.

Add local tables for:

- node identity
- configuration
- node state
- Core enrollment information

Requirements:

- Node state must survive Agent restart.
- The Agent should initialize missing local state safely.
- The node must have an opaque node ID.
- Do not store Core administrator secrets locally.
- Add database health to Agent health reporting.

Acceptance criteria:

- Restarting Agent preserves node identity.
- Recreating only the Agent container does not generate a new identity.
- Database failures appear clearly in Node Agent health.

---

# PHASE 3 — Node State Machine

## Prompt 09 — Implement the ArkNet Node State Machine

Implement the Node Agent state machine.

Supported states:

- `PROVISIONING`
- `ENROLLMENT_PENDING`
- `CONNECTED`
- `SYNCHRONIZING`
- `MESH`
- `DEGRADED`
- `OFFLINE`
- `RECONNECTING`
- `QUARANTINED`
- `REVOKED`
- `ERROR`

Requirements:

- Define valid state transitions explicitly.
- Prevent invalid transitions.
- Persist current state in PostgreSQL.
- Record state transition timestamps.
- Expose current state in `/api/v1/status`.
- Add comprehensive tests for valid and invalid transitions.

Do not add network detection yet.

Acceptance criteria:

- State transitions are deterministic and tested.
- Invalid transitions fail cleanly.
- State survives restart.

---

# PHASE 4 — Enrollment

## Prompt 10 — Implement Node Enrollment Request

Implement outbound enrollment from Node Agent to Core.

Flow:

1. Agent has a locally generated node ID.
2. Agent sends an enrollment request to Core.
3. Core creates a pending enrollment record.
4. Node enters `ENROLLMENT_PENDING`.
5. Repeated requests must not create duplicate node records.

Information sent should be minimal:

- node ID
- ArkNet version
- basic capability information placeholder

Do not send:

- public WAN IP as application metadata
- MAC address
- hardware serial number
- operator hostname
- arbitrary filesystem data

Acceptance criteria:

- Fresh node can submit enrollment.
- Core stores exactly one pending node record.
- Agent becomes `ENROLLMENT_PENDING`.

---

## Prompt 11 — Add Core Enrollment Administration Endpoints

Add Core administration endpoints for:

- list pending nodes
- approve node
- reject node

Requirements:

- Keep these under a clearly separate administrative API namespace.
- Administrative authorization can initially use a development-only mechanism, but structure the code so real authentication can replace it.
- Approval must update the node trust state.
- Rejection must be persisted.
- Add tests.

Do not build the full Core admin UI yet.

Acceptance criteria:

- Pending node can be approved.
- Pending node can be rejected.
- Invalid or duplicate actions return safe errors.

---

## Prompt 12 — Complete Node Enrollment Handshake

Complete the Node-side enrollment lifecycle.

After approval:

- Node learns it has been approved.
- Node persists federation enrollment state.
- Node moves from:
  - `ENROLLMENT_PENDING`
  - to `CONNECTED`
- Rebooting the node must not require re-enrollment.

Requirements:

- Node credentials must be unique to that node.
- One node's credential must not authenticate another node.
- Do not implement operator identity yet.

Acceptance criteria:

- Node survives restart without re-enrollment.
- Node A credentials fail when presented as Node B.
- Approved node reaches `CONNECTED`.

---

# PHASE 5 — Federation Heartbeat

## Prompt 13 — Implement Node Heartbeats

Implement periodic Node Agent heartbeats to Core.

Heartbeat should include only required federation metadata:

- node ID
- node version
- node state
- basic health summary
- capability placeholder

Core should update:

- last-seen timestamp
- current state
- reported version

Requirements:

- Use outbound Node → Core communication.
- Temporary Core failures must not terminate the Agent.
- Agent should transition appropriately toward `DEGRADED` or `OFFLINE`.
- Use retry with reasonable backoff.
- Do not retry aggressively.

Acceptance criteria:

- Core displays fresh last-seen state.
- Stopping Core causes appropriate node state change.
- Restoring Core allows the node to reconnect automatically.

---

## Prompt 14 — Implement Reconnect Behavior

Implement automatic recovery after Core connectivity loss.

Expected flow:

`MESH → DEGRADED → OFFLINE → RECONNECTING → CONNECTED/MESH`

Requirements:

- Local Agent API remains available while offline.
- Database remains available.
- Core restart requires no manual node action.
- Use bounded exponential backoff with jitter.
- State transitions should be visible in logs and UI.

Acceptance criteria:

- Stop Core.
- Node becomes offline without crashing.
- Restart Core.
- Node reconnects automatically.
- No enrollment reset occurs.

---

# PHASE 6 — Node Dashboard

## Prompt 15 — Build the Functional Node Dashboard

Expand the Node UI dashboard.

Display:

- Node ID
- ArkNet version
- Node state
- enrollment state
- Core connectivity
- last successful Core contact
- database health
- Agent health

Requirements:

- Human-friendly status labels.
- Do not expose internal secrets.
- Clearly distinguish:
  - local services healthy
  - federation offline
- Avoid generic errors such as only displaying `502`.

Acceptance criteria:

A Node Operator can answer:

- Is my node running?
- Is Core reachable?
- Is my node enrolled?
- When did it last contact Core?

without using the CLI.

---

# PHASE 7 — Jellyfin

## Prompt 16 — Add Jellyfin to the Node Stack

Add Jellyfin to the ArkNet Node Docker Compose stack.

Requirements:

- Persist Jellyfin configuration.
- Mount ArkNet-managed media storage read-only where practical.
- Do not expose ArkNet PostgreSQL to Jellyfin.
- Agent should be able to determine Jellyfin health.
- UI should show Jellyfin status.
- Jellyfin must start even if ArkNet Core is unavailable.

Create initial paths conceptually like:

- `/srv/arknet/appdata`
- `/srv/arknet/media`
- `/srv/arknet/transfers`

Document permissions.

Acceptance criteria:

- Jellyfin starts.
- Jellyfin survives host/container restart.
- Core outage does not stop Jellyfin.
- UI reports Jellyfin health.

---

## Prompt 17 — Create the Application Adapter Interface

Refactor Jellyfin integration behind a generic ArkNet application adapter interface.

Support operations such as:

- health
- start
- stop
- restart
- configuration status
- version
- enabled state

Requirements:

- Jellyfin should be the first implementation.
- Do not over-engineer a dynamic plugin marketplace.
- Keep the interface suitable for future applications like Seerr and Calibre-Web.

Acceptance criteria:

- Existing Jellyfin behavior still works.
- Agent application-management logic no longer depends directly on Jellyfin-specific code outside the Jellyfin adapter.

---

# PHASE 8 — Media Catalog

## Prompt 18 — Add Core Media Objects

Create the initial Core media catalog data model.

Fields should include:

- opaque media object ID
- media type
- title
- size
- state
- BitTorrent v2 info hash placeholder
- created/updated timestamps

Requirements:

- Do not use filesystem paths as global identity.
- Support at minimum:
  - `ACTIVE`
  - `REMOVED`
- Add API endpoints for nodes to retrieve authorized catalog objects.

Acceptance criteria:

- Core can create and retrieve test media objects.
- Node can fetch the available catalog.
- Removed objects remain identifiable as removed.

---

## Prompt 19 — Add Node Media Selection

Add Media selection to Node Agent and UI.

UI should show ArkNet catalog objects with:

- title
- size
- local state
- `Make Available Locally` action

Initial local states:

- `NOT_PRESENT`
- `QUEUED`
- `DOWNLOADING`
- `AVAILABLE`
- `REMOVED_RETAINED`
- `ERROR`

Requirements:

- Selection is stored locally.
- Do not implement automatic caching yet.
- Respect a configurable storage allocation.

Acceptance criteria:

- Operator can queue an ArkNet media object.
- Queue persists across restart.

---

# PHASE 9 — BitTorrent v2 Private Swarm

## Prompt 20 — Define the ArkNet Transfer Service Interface

Before implementing BitTorrent, define a small local API between `arknet-agent` and `arknet-transfer`.

Required operations:

- add authorized object transfer
- query transfer status
- pause
- resume
- cancel
- report completion
- report verification failure

Requirements:

- Agent owns ArkNet authorization decisions.
- Transfer service owns byte movement.
- Transfer service must not receive broad Core administrative credentials.
- Define API structures and error behavior.
- Implement a mocked transfer service first.

Acceptance criteria:

- Agent can drive a fake transfer lifecycle through the interface.
- UI can display progress from the mock service.

---

## Prompt 21 — Implement the BitTorrent v2 Transfer Container

Implement `arknet-transfer` using a mature libtorrent-based implementation.

Requirements:

- BitTorrent v2 only.
- Disable:
  - public DHT
  - PEX
  - local peer discovery
  - arbitrary public trackers
- Only ArkNet-authorized tracker configuration may be used.
- Persist resume information.
- Download to ArkNet transfer staging storage.
- Validate completed data before marking complete.
- Expose only the local ArkNet Transfer API.

Do not integrate public torrent ecosystems.

Acceptance criteria:

- A test v2 torrent downloads successfully.
- Restarting the transfer container resumes the transfer.
- Public DHT and PEX remain disabled.

---

## Prompt 22 — Implement the Private ArkNet Tracker

Create a dedicated private tracker service for ArkNet.

Requirements:

- Separate service from the main Core API.
- BitTorrent v2 compatible.
- No public swarm functionality.
- Require ArkNet-issued authorization.
- Tracker itself should not need access to:
  - global user profiles
  - watch history
  - local usernames
- Log minimum necessary metadata.

Acceptance criteria:

- Unauthorized node cannot join a swarm.
- Authorized node can join.
- Knowing only the info hash and tracker endpoint is insufficient.

---

## Prompt 23 — Implement Swarm Authorization Tokens

Implement short-lived swarm authorization issued by ArkNet Core.

Token should be scoped to:

- node
- media object/swarm
- allowed actions:
  - download
  - seed
- expiration

Requirements:

- Cryptographically signed or otherwise securely verifiable.
- No global-user identity in the token.
- Tracker can validate authorization without broad Core DB access.
- Expired token must fail.
- Node revoked by Core must not receive new authorization.

Acceptance criteria:

Tests prove:

- wrong node fails
- wrong swarm fails
- expired token fails
- correct token succeeds

---

## Prompt 24 — Implement Core HTTP Web Seed

Provide an HTTP web-seed endpoint for ArkNet media objects.

Requirements:

- Compatible with the v2 transfer metadata.
- Support efficient range requests.
- Require appropriate authorization.
- Do not expose arbitrary filesystem browsing.
- Map opaque ArkNet object IDs to approved content internally.

Acceptance criteria:

- Transfer engine can retrieve media pieces from Core web seed.
- Unauthorized arbitrary paths cannot be retrieved.

---

## Prompt 25 — Complete Core-to-Node Media Transfer

Connect the full path:

1. Operator chooses `Make Available Locally`.
2. Agent requests swarm authorization.
3. Core authorizes object.
4. Transfer engine starts.
5. Private tracker/web seed provides source.
6. Transfer resumes if interrupted.
7. Object verifies successfully.
8. File moves into local ArkNet media storage.
9. Jellyfin refreshes.
10. Node reports replica available.

Acceptance criteria:

- Large test file completes.
- WAN interruption is recoverable.
- Jellyfin can play completed content.
- Core knows the object is available on the node.

---

# PHASE 10 — Multi-Node Swarm

## Prompt 26 — Add Opt-In Peer Serving

Add Node Operator setting:

`Peer Serving`

Default: OFF.

Requirements:

- Setting managed through ArkNet UI.
- Persist locally.
- Report capability/participation to Core.
- If OFF, Node should not advertise itself as an ArkNet seed.
- Receiving content must remain possible while serving is off.

Acceptance criteria:

- Fresh node does not serve peers.
- Operator can explicitly enable serving.
- Disabling serving removes the node from future source eligibility.

---

## Prompt 27 — Prove Multi-Node Private Swarm Transfer

Using Core, Node A, and Node B:

1. Replicate a test object to Node A.
2. Enable peer serving on Node A.
3. Request the same object from Node B.
4. Allow Node B to obtain pieces from:
   - Node A
   - Core web seed

Record:

- object size
- Core egress
- Node A upload
- Node B transfer rate
- completion time

Acceptance criteria:

- Node A contributes verified pieces.
- Core egress is lower than a Core-only download.
- Node B still completes if Node A disappears mid-transfer.

Document results.

---

# PHASE 11 — Identity

## Prompt 28 — Implement ArkNet Global Users

Create Core global user accounts.

Requirements:

- Stable opaque user IDs.
- ArkNet-owned identity.
- Secure password hashing.
- No plaintext password storage.
- Account status:
  - active
  - disabled
- Authentication API.
- Sessions/tokens must have reasonable expiration.

Do not integrate external identity providers.

Acceptance criteria:

- User registration/admin creation works according to chosen MVP mechanism.
- Valid login succeeds.
- Invalid credentials fail safely.
- Disabled user cannot establish new sessions.

---

## Prompt 29 — Implement Privacy-Preserving Node Invitations

Implement node membership invitations.

Operator can create an invitation with:

- node ID
- membership role
- expiration
- maximum uses

Requirements:

- Operator cannot search or enumerate the global ArkNet user directory.
- Invitation secret must be unguessable.
- Store only a secure hash of the secret where practical.
- User authenticates to Core when accepting.
- Successful acceptance creates node membership.

Acceptance criteria:

- Invitation works once according to policy.
- Expired invitation fails.
- Operator cannot use the API to browse unrelated global users.

---

## Prompt 30 — Implement Pairwise Node Membership IDs

For each global user/node relationship, generate a unique random pairwise membership ID.

Requirements:

- Same global user has different IDs on different nodes.
- Nodes never receive the Core global user ID unless explicitly required by future design.
- Pairwise IDs must not be deterministically reversible by nodes.
- Core maintains the mapping privately.

Acceptance criteria:

User A on Node 1 and Node 2 produces two unrelated membership IDs.

---

## Prompt 31 — Implement Node-Local Users

Allow Node Operators to create local-only users.

Requirements:

- Stored only on the Node.
- Never synchronized to Core.
- Valid only on that node.
- Usable while offline.
- Distinguishable from global users in UI.
- Matching usernames must not cause automatic account merging.

Acceptance criteria:

- Local user exists when Core is offline.
- Core never receives the user.
- Same username on another node is a different account.

---

# PHASE 12 — Offline Identity

## Prompt 32 — Implement Cached Global Authorization

Allow previously approved global users to authenticate to their home/authorized node while Core is temporarily offline.

Requirements:

- Do not replicate the plaintext global password database.
- Store bounded offline-verification material.
- Include:
  - membership identity
  - allowed node
  - permissions
  - validation timestamp
  - expiration
- Core remains authoritative when reachable.
- Security-sensitive global changes remain unavailable offline.

Acceptance criteria:

- Previously authorized user can log in during simulated Core outage.
- Unknown global user cannot.
- Expired cached authorization fails.
- Reconnecting refreshes authorization state.

---

# PHASE 13 — Media Removal

## Prompt 33 — Implement Removed-Media Semantics

Implement the ArkNet removal behavior.

When Core marks an object `REMOVED`:

Node should transition its local ArkNet state to:

`REMOVED_RETAINED`

if a local copy exists.

Requirements:

- Local file is not deleted automatically.
- Object can no longer be newly replicated.
- Object is no longer peer-serving eligible.
- Node Operator may delete the retained local copy manually.
- Core should track only the ArkNet object status, not arbitrary filesystem details.

Acceptance criteria:

- Remove test object from Core.
- Node keeps local file.
- Node stops advertising it to ArkNet peers.
- Jellyfin behavior is documented.

---

# PHASE 14 — Home Node and Routing

## Prompt 34 — Implement Primary Home Node

Allow each global user to have one primary home node.

Requirements:

- Relationship stored in Core.
- User may belong to several nodes but has one home node.
- Changing home node requires explicit action.
- Node Operators cannot discover the user's other node memberships.

Acceptance criteria:

- Core returns a user's preferred home node to authorized routing logic.
- Other community nodes receive no unrelated membership data.

---

## Prompt 35 — Implement Media Availability Routing Decision

Create a Core routing-decision API.

Inputs:

- authenticated global user
- requested media object

Decision logic:

1. Is home node healthy?
2. Does home node report verified local availability?
3. If yes: prefer home node.
4. Otherwise: select Core.

Return only minimum necessary routing data.

Acceptance criteria:

- Local available media routes toward home node.
- Missing media routes toward Core.
- Offline home node routes toward Core.

---

## Prompt 36 — Implement Media-Miss Events

When playback falls back to Core because the user's home node lacks the media, create a media-miss event.

Node policy modes:

- `MANUAL`
- `ASK`
- `AUTOMATIC`

MVP default:

`ASK`

Requirements:

- In ASK mode, notify the Node Operator.
- UI offers:
  - `Make Available Locally`
  - `Ignore`
- Automatic mode may be implemented if simple, but must obey:
  - storage quota
  - bandwidth policy
  - object availability

Acceptance criteria:

- Core fallback does not delay playback.
- Operator can queue missed media from the notification.

---

# PHASE 15 — Releases and Updates

## Prompt 37 — Define ArkNet Release Manifest

Define a versioned whole-stack ArkNet Node release manifest.

It should identify compatible versions of:

- arknet-agent
- arknet-ui
- arknet-transfer
- PostgreSQL
- Jellyfin
- Tailscale
- Compose definitions

Requirements:

- Human-facing release version such as `0.1.0`.
- Stable and development channels.
- Manifest should be machine-readable.
- Individual community operators should not need to manage component versions.

Acceptance criteria:

- Node Agent can parse its current release manifest.
- UI shows current ArkNet version.

---

## Prompt 38 — Implement Update Discovery

Implement Node update discovery.

UI should display:

- current ArkNet version
- available version
- update channel
- automatic update status

Requirements:

- Automatic updates default ON.
- Operator can disable them.
- Disabling must persist.

Do not implement destructive automatic updates until discovery logic is reliable.

Acceptance criteria:

- Node detects a newer test release.
- Opt-out persists across restart.

---

## Prompt 39 — Implement Whole-Stack Upgrade

Implement the supported upgrade workflow.

Requirements:

- Validate prerequisites.
- Pull required images.
- Apply the versioned Compose release.
- Preserve persistent volumes.
- Validate ArkNet Agent and Jellyfin afterward.
- Report failure clearly.
- Provide a recovery strategy where practical.

Acceptance criteria:

Upgrade a test Node from one ArkNet development release to another without manually editing Docker resources.

---

# PHASE 16 — Installer

## Prompt 40 — Build the ArkNet Node Bootstrap Installer

Create a bootstrap installer for a fresh supported Ubuntu LTS machine.

Responsibilities:

- Verify supported OS.
- Verify architecture.
- Verify minimum memory/storage.
- Install or validate Docker.
- Create ArkNet directories.
- Download the selected stable ArkNet release.
- Start the Node stack.
- Direct operator to the local setup UI.

Requirements:

- Re-running installer should be safe where practical.
- Do not require repository cloning.
- Do not require manual Compose editing.
- Provide actionable errors.

Acceptance criteria:

A fresh VM reaches `ENROLLMENT_PENDING` by following the documented installation process.

---

# PHASE 17 — Diagnostics

## Prompt 41 — Implement Node Diagnostics

Build a diagnostics view in the ArkNet UI.

Display:

- Agent health
- Jellyfin health
- PostgreSQL health
- Core connectivity
- Tailscale status
- private tracker reachability
- storage capacity
- ArkNet media storage usage
- active transfers
- ArkNet version

Requirements:

Provide human-readable explanations such as:

`Federation Offline — Local services are still operating.`

instead of generic networking errors.

Acceptance criteria:

A Node Operator can diagnose common service/connectivity failures without SSH.

---

# PHASE 18 — Security Hardening

## Prompt 42 — Perform ArkNet MVP Security Review

Review the MVP implementation against these requirements:

- Nodes cannot enumerate other community nodes unnecessarily.
- Operators cannot enumerate global users.
- Local users never synchronize.
- Pairwise membership IDs cannot be trivially correlated across nodes.
- Node credentials are unique.
- Tracker requires swarm authorization.
- DHT/PEX/LSD remain disabled.
- Revoked nodes cannot obtain new swarm authorization.
- Core APIs use least privilege.
- Secrets are not exposed in logs/UI.
- Community nodes do not receive Core administrative credentials.

Do not merely document findings.

Fix high-risk issues that are reasonably within scope and create tracked follow-ups for larger architectural changes.

Produce a concise security review document.

---

# PHASE 19 — MVP Validation

## Prompt 43 — Execute the ArkNet MVP End-to-End Test

Build and execute an end-to-end MVP test using:

- ArkNet Core
- Node A
- Node B

Validate:

1. Fresh node installation.
2. Enrollment.
3. Node restart.
4. Global user.
5. Local-only user.
6. Pairwise node membership.
7. Jellyfin.
8. Media selection.
9. BitTorrent v2 Core → Node transfer.
10. Interrupted transfer/resume.
11. Node A → Node B swarm contribution.
12. Offline Jellyfin playback.
13. Cached global authentication.
14. Core reconnect.
15. Removed-media retention.
16. Node revocation.
17. Update discovery.
18. Whole-stack upgrade.
19. Operator cannot enumerate global users.
20. Node A cannot arbitrarily access Node B.

Record failures and fix blockers before calling the MVP complete.

Produce an MVP validation report with PASS/FAIL results.

---

# PHASE 19A — MVP Validation Remediation

Prompt 43 was too large for one session. Its first execution found blockers.
Run these prompts in order, one per session. Each prompt is self-contained.
Every prompt finishes only when the stated tests have actually run and passed.

Status when this phase was written:

- Done and unit-tested:
  - Revocation detection. A 403 heartbeat triggers a signed enrollment-status
    check, and only a confirmed `REVOKED/BLOCKED` result moves the Node to
    terminal `REVOKED`. See `node-agent/internal/heartbeat/revocation_test.go`.
  - Core issues `seed` tracker grants only to eligible sources: a verified
    replica, Peer Serving on, an approved and trusted Node, and an ACTIVE object.
    See `core/seed_grants_test.go`. A seed grant can fetch torrent metadata, but
    never web-seed payload.
- Item 1, fresh install: `scripts/check-node-install-vm.py` passed once.
- Items 17/18, update discovery and upgrade: `scripts/check-node-upgrade-stack.py`
  fails at `release A installed and running`.
- `scripts/check-mvp-e2e.py` is the Core + Node A + Node B harness for items
  2–16, 19 and 20. Its first run failed during Core `docker compose up`, so
  every item is `NOT_RUN`.
- Item 11 blocker: the product has no seeding path. The transfer worker
  (`transfer/worker.py`) removes a torrent after completion. The Node Agent
  never requests a seed grant. The old swarm check used a test-only seeder.
- Open security follow-ups that affect validation are listed in
  `docs/security-follow-ups.md`:
  - ARKSEC-001: the global password still transits the Node (item 13).
  - ARKSEC-002: the Node management API is unauthenticated (item 20).

Rules for every prompt in this phase:

- The repository has no `.git` directory. Verify changes by reading files and
  running tests.
- Never print secrets. Generate Core admin tokens at runtime.
- Host Python has no `libtorrent`. Run worker tests inside the transfer or
  libtorrent container.
- Keep logs in `~/.hermes/cache/scratch/mvp-e2e/`. Report the `EXIT` line of
  each log, not the wrapper process exit code.
- Before claiming success, run gofmt, `go test ./...`, and `go vet ./...` for
  each Go module you touched.

## Prompt 43A — Make the MVP E2E Harness Start Core

Context: `scripts/check-mvp-e2e.py` reuses helpers from
`scripts/check-core-to-node-transfer-stack.py`. Its first run failed at
`docker compose ... deploy/core/... up --build`, before any item was checked.
One suspected cause: the harness sets `ARKNET_CORE_WEB_SEED_SOURCE`, but Core
reads `ARKNET_CORE_WEB_SEED_ROOT` (`core/web_seed.go`). Confirm or reject this
from the sanitized compose logs before changing anything.

Task:

1. Reproduce the failure and capture sanitized Core compose logs.
2. Fix only the harness configuration. Compare it with the working Core setup
   in `check-core-to-node-transfer-stack.py` and `check-multi-node-swarm-stack.py`.
3. Add a `--setup-only` flag that stops after printing `SETUP:` and then cleans up.

Acceptance criteria:

- `python3 scripts/check-mvp-e2e.py --setup-only` exits 0.
- The output contains `SETUP:` and `PASS: exact scoped cleanup`.
- No containers, volumes, networks or temporary directories are left behind.

## Prompt 43B — Diagnose and Fix the Node Upgrade Check

Context: `scripts/check-node-upgrade-stack.py` fails at
`release A installed and running`. The generated `release.json` for
`0.0.2-dev.1` parses correctly with `node-agent/internal/release`.
Validation items 17 and 18 depend on this check.

Task:

1. Reproduce the failure and collect sanitized Agent and installer logs.
2. Find the root cause and decide whether it is a harness bug or a product bug.
3. Fix the root cause with the smallest change. Product bugs need a regression test.

Acceptance criteria:

- `python3 scripts/check-node-upgrade-stack.py` ends with `EXIT 0`.
- The output shows that update discovery found release B and that the
  whole-stack upgrade reached B, with data preserved.

## Prompt 43C — Add Seed Mode to the Transfer Worker

Context: `transfer/worker.py` speaks a strict JSON-lines protocol with
`transfer/backend/client.go`. After a download completes, it removes the torrent
so that receiving never implies serving. ArkNet media files live at
`/srv/arknet/media/<object_id>.mp4`, which `transfer/` can already see. Core's
seed grants authorize announcing with `left=0`.

Task:

1. Add worker operations `seed` and `unseed`:
   - `seed` takes exactly these fields: `object_id`, `size`, `info_hash_v2`,
     `tracker_url`, `torrent_url`, `file_name`.
   - `unseed` takes `object_id`.
2. Seeds are runtime-only. They are never persisted and are gone after a
   restart.
3. Seeding rules:
   - Never write to the media directory. Use upload mode, so a seed can never
     download.
   - Map the single torrent file to `file_name`.
   - Run a full hash recheck before serving. Drop the seed if the recheck does
     not complete.
4. Validate `file_name` strictly against `media_<64 hex>.mp4`.
5. Keep DHT, PEX and LSD disabled. Keep tracker and web-seed overrides in place.
   A seed must have no web seeds.
6. A repeated `seed` for the same object refreshes only the tracker URL and
   triggers a re-announce.

Acceptance criteria:

- Container-run worker tests cover all of the following:
  - a valid seed reaches the seeding state;
  - a corrupt or short file is rejected without any write;
  - an invalid `file_name` or URL is rejected;
  - `unseed` removes the torrent;
  - a restart leaves no seeds.
- Existing worker tests still pass.

## Prompt 43D — Expose Seeds Through the Transfer Service API

Context: this depends on 43C. The transfer service (`transfer/transfers.go`,
`transfer/server.go`, `transfer/backend/client.go`) requires the local bearer token.

Task:

1. Add three routes:
   - `POST /api/transfer/v1/seeds`, which uses the same exact-field validation
     style as transfers and the same tracker/torrent URL validators;
   - `GET /api/transfer/v1/seeds`, which returns object IDs and state only;
   - `DELETE /api/transfer/v1/seeds/{object_id}`.
2. Add the matching methods to the backend client.
3. Add matching methods (`Seed`, `Unseed`, `ListSeeds`) to the Node Agent
   client `node-agent/internal/transfer/client.go`, and update its fake.

Acceptance criteria:

- Go tests in `transfer/` and `node-agent/internal/transfer` cover all of:
  - authentication;
  - unknown fields;
  - invalid URLs and IDs;
  - list and delete behaviour.
- No URL, token or hash appears in logs.

## Prompt 43E — Node Agent Peer-Serving Seed Reconciler

Context: this depends on 43D. The Agent has these pieces:

- `catalog.Grant`, which today accepts only `["download"]`;
- the local Peer Serving setting (`node-agent/peer_settings.go`, which reads the
  `PeerServing` store method);
- `transferReconciler` in `node-agent/transfer_finalizer.go`.

Core returns 403 from the grant endpoint when the Node is not an eligible
source.

Task:

1. Add `catalog.SeedGrant`. It sends `{"actions":["seed"]}`, accepts only
   `actions == ["seed"]`, and requires `torrent_url`. A response containing
   `web_seed_url` is an error.
2. Add a seed reconciler that runs on the existing reconcile interval.
3. While Peer Serving is ON:
   - for each AVAILABLE, ACTIVE, verified object, request a seed grant and call
     `Seed`;
   - refresh the grant before it reaches half its lifetime.
4. Call `Unseed` for an object in any of these cases:
   - Peer Serving is OFF (stop all seeds);
   - the object is REMOVED or no longer AVAILABLE;
   - Core refuses the seed grant (401 or 403);
   - the Node is not CONNECTED/APPROVED, including a REVOKED Node.

Acceptance criteria:

- Unit tests cover:
  - Peer Serving ON starts seeds;
  - turning it OFF stops all seeds within one interval;
  - a Core 403 stops that seed;
  - REVOKED stops all seeds;
  - removed media is unseeded;
  - no seed is ever requested for media that is not AVAILABLE.
- This closes ARKSEC-003 if its acceptance criteria are met. Update
  `docs/security-follow-ups.md` and `docs/peer-serving.md`.

## Prompt 43F — Prove Product Node A → Node B Swarm Contribution

Context: this depends on 43E. `scripts/check-multi-node-swarm-stack.py`
currently uses a test-only seeder adapter.

Task:

1. Replace the adapter with Node A's real transfer service, seeding through the
   43E reconciler after the operator enables Peer Serving.
2. Prove that Node B received payload from Node A. Keep Core web-seed payload
   metering. Either make the web seed unreachable after metadata is fetched, or
   show that Core-metered payload is below the object size.
3. Prove the negative case: with Peer Serving OFF, Node A announces nothing and
   serves nothing.

Acceptance criteria:

- The check ends with `EXIT 0`.
- The evidence JSON states the product path, with no test adapter.
- `scripts/multi_node_swarm_evidence_test.py` is updated, and it passes.

## Prompt 43G — Run the Full MVP E2E and Fix Failures One at a Time

Context: this depends on 43A and 43F. Run the harness on the current tree:

```
python3 scripts/check-mvp-e2e.py --results ~/.hermes/cache/scratch/mvp-e2e/e2e.json
```

Update the harness's item 11 to use the 43F product path.

Task:

1. Run the harness.
2. For each FAIL, decide whether the cause is the harness or the product.
3. Fix exactly one root cause, add a regression test, and rerun.
4. Stop after three fix cycles and report what remains.

Acceptance criteria:

- Every item in the results JSON is PASS, or each remaining FAIL has a
  root-cause note and a proposed fix.
- Cleanup reports `PASS: exact scoped cleanup`.

## Prompt 43H — Rerun Install and Upgrade Checks on the Final Tree

Task:

1. On the same source tree as the final 43G run, run
   `scripts/check-node-install-vm.py` (item 1) and
   `scripts/check-node-upgrade-stack.py` (items 17 and 18).
2. Record the `EXIT` line of each log.

Acceptance criteria:

- Both checks end with `EXIT 0`, or each failure is fixed or recorded with its
  root cause.

## Prompt 43I — Write the MVP Validation Report

Task:

1. Write `docs/mvp-validation-report.md` and link it from `docs/README.md`.
2. Include a table with all 20 Prompt 43 items. Each row gives:
   - PASS or FAIL;
   - the check script and step that proved it;
   - the run date;
   - a short piece of evidence.
3. List the blockers fixed during Phase 19A.
4. List known limitations, linking each one to its ARKSEC ID. At minimum:
   - ARKSEC-001: the global password transits the Node (item 13);
   - ARKSEC-002: the Node management API is unauthenticated (item 20);
   - ARKSEC-006: active transfers on a revoked Node are not forcibly terminated.
5. Give a verdict: is the MVP functionally complete, and is it ready for
   hostile multi-operator production? Answer the two questions separately.
6. Update `docs/mvp-security-review.md` and `docs/security-follow-ups.md`
   statuses where 43C–43F changed them.

Acceptance criteria:

- Every PASS is backed by a log from a run on the final tree.
- No item is marked PASS from unit tests alone where an E2E check exists.

---

# OPTIONAL HERMES META-PROMPT

Use this at the beginning of a fresh Hermes session when it needs project context:

You are working on ArkNet, a privacy-focused distributed self-hosting platform.

ArkNet consists of a centrally coordinated Core and independently owned community Nodes.

Core provides:

- federation trust
- global identity
- node enrollment
- media catalog
- authorization
- swarm coordination
- routing
- releases

Community Nodes provide:

- local Jellyfin
- local media
- local-only users
- cached access for authorized global users
- offline functionality
- private swarm participation

Architecture principles:

- Zero trust.
- Community nodes are independently controlled and considered untrusted infrastructure.
- Nodes primarily initiate connections outbound.
- Node-to-node access is denied by default.
- Privacy and pseudonymity are important.
- Node operators cannot browse the global user directory.
- Global users use pairwise pseudonymous IDs on each node.
- Local users never synchronize to Core.
- Core tracks ArkNet-managed objects only, not arbitrary node files.
- Media uses BitTorrent v2 private swarms.
- Public DHT, PEX, and local peer discovery are disabled.
- Core provides an HTTP web seed fallback.
- Peer serving is opt-in.
- Private relay is preferred by default.
- Community Nodes use Ubuntu LTS, Docker Engine, and Docker Compose.
- Core uses Docker initially and may migrate to K3s later.
- Backend services are primarily Go.
- PostgreSQL stores ArkNet platform state.
- Node management UI is React/TypeScript.
- ArkNet uses versioned whole-stack releases.
- Avoid unnecessary microservices and operational complexity.

When implementing a task, inspect the existing code first, preserve established architecture, make the smallest clean change that satisfies the acceptance criteria, add tests, and do not implement future features unless required by the current task.