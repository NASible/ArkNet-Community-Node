# ArkNet Hermes Prompt Pack — Phase 20: Trust Boundaries

## How to use this pack

Phase 19A ended with the MVP functionally complete: 20/20 Prompt 43 items pass
on the final tree (see [MVP validation report](../mvp-validation-report.md)).
It is **not** ready for hostile multi-operator production. This phase closes
the open release blockers in [Security follow-ups](../security-follow-ups.md),
in dependency order.

Give Hermes **one prompt at a time, in order**. Each prompt is self-contained.
A prompt is finished only when its stated tests have actually run and passed.

Rules for every prompt in this phase (in addition to the
[MVP pack rules](hermes-mvp-prompts.md#how-to-use-this-pack)):

- Inspect the existing code first. Make the smallest clean change that meets
  the acceptance criteria. Do not build later prompts' features early.
- Never print secrets. Generate test credentials at runtime.
- Product bugs need a regression test that fails before the fix.
- Host Python has no `libtorrent`. Run worker tests inside the transfer image.
- Before claiming success, run gofmt, `go test ./...` and `go vet ./...` for
  every Go module you touched, and `npm test -- --run && npm run build` in
  `node-ui/` if you touched it.
- Report the `EXIT` line of each check log, not the wrapper's exit code.
- Any change to runtime code invalidates earlier final-tree evidence. A prompt
  that claims an ARKSEC item is closed must cite a run on the tree it changed.
- Design prompts produce a document only. They must list open questions for
  the user and must not start implementation.

## Decisions needed from the user

Some prompts depend on these decisions. Each has a recommended default. A
prompt that depends on an undecided item must stop and ask, not assume.

| ID | Decision | Recommended default | Needed by |
| --- | --- | --- | --- |
| D1 | Where automated checks run | A local one-command gate (`scripts/check-all.sh`), plus a self-hosted runner later. Hosted CI cannot run the Lima VM or the Docker-heavy E2E reliably. | 44A |
| D2 | How Node operators authenticate | **Confirmed 2026-10-27:** Node-local operator accounts that work offline, password plus TOTP first (passkeys later), one operator role. Optional OIDC (for example Authentik) only as an add-on for operators who run an IdP. Community Nodes must not depend on one person's IdP. Details and the other Prompt 45 answers: [operator authentication decisions](../node-operator-authentication.md#decisions). | 45 |
| D3 | How long global users can log in offline | **Confirmed 2026-10-29:** at most 30 days on a Node since the last online verification. Core may set a shorter limit per Node or per user. See [global login decisions](../global-login-assertions.md#decisions). | 46 |
| D4 | First login for a global user on a Node | Redirect to Core, which requires Core to be reachable once per Node per offline window. Adopted with the Prompt 46 decisions (Core login page, Node-registered callback). | 46 |
| D5 | Who operates the private relay | **Confirmed 2026-11-03:** the ArkNet operator runs most relays; community-run relays only as one hop of a two-hop path through an operator relay, end-to-end encrypted to it, admitted by Core (48E). No direct mode in the first release. See [relay decisions](../private-relay-design.md#decisions-confirmed-2026-11-03). | 48 |

---

# PHASE 20A — Foundation

## Prompt 44 — Version Control Baseline

Context: the checkout has no `.git` directory. Phase 19A evidence relied on
hand-made tree hashes (`7b71aee9…` for the final MVP tree).

Task:

1. Initialize git. Review `.gitignore` against the security hygiene rules:
   no `.env`, keys, databases, logs, media, `deploy/live-test/secrets/`,
   `node_modules`, `dist` or `.arknet-*` work directories.
2. Run `python3 scripts/check-security-hygiene.py` before the first commit.
3. Commit the tree and tag it `v0.1.0-mvp`. Record in the tag message that it
   corresponds to the Phase 19A final tree.
4. Document the branch and tag conventions in `docs/development/`.

Acceptance criteria:

- `git status` is clean after the commit, and the hygiene check passes.
- No ignored-category file is tracked: `git ls-files` contains nothing under
  `deploy/live-test/secrets/` and no `*.env`, `*.key` or `*.db` files.
- Hashing the committed files with `tree-hash.sh` from the final-tree evidence
  directory gives the Phase 19A digest, or the report explains every
  difference.

## Prompt 44A — One-Command Gate and E2E Failure Diagnostics

Context: Phase 19A needed three investigation cycles partly because a failing
E2E left no transfer state or service logs behind. Depends on D1.

Task:

1. Add `scripts/check-all.sh`. It runs the component gate (gofmt, go
   test/vet for all modules, `node-ui` tests and build, harness tests, the
   hygiene check, and worker tests in the transfer image). It runs the
   Docker E2E checks only with `--e2e`.
2. On any failure, `check-mvp-e2e.py` and `check-multi-node-swarm-stack.py`
   must write a sanitized diagnostics bundle before cleanup: transfer status
   per Node, worker events, and the last service log lines with known
   secrets redacted.
3. If D1 selects a runner, add its configuration.

Acceptance criteria:

- `scripts/check-all.sh` ends with `EXIT 0` on the current tree.
- A forced harness failure produces a diagnostics bundle with no known secret
  in it, verified by a test.

## Prompt 44B — Small Fixes and Live-Test Credential Rotation

Task:

1. Make `transfer/server.go`'s `server_started` log report the real mode
   (`mock:false` for libtorrent). Add a test.
2. Add `deploy/live-test/rotate-credentials.py`. It rotates the live-test
   global-user, Jellyfin administrator, Core admin token, database and
   transfer-token secrets in place. It updates `secrets/runtime.json` and
   `secrets/access.txt`, never prints a secret, and verifies login with every
   new credential and rejection of every old one.
3. Run it against the live-test group, then run `verify-deployment.py`.

Acceptance criteria:

- Both scripts end with `EXIT 0`, and old credentials are rejected.
- This addresses ARKSEC-009 for the live-test group only. Update its status
  text to say so.

---

# PHASE 20B — Node Management Plane (ARKSEC-002)

## Prompt 45 — Design: Node Operator Authentication

Context: every Agent management route (`node-agent/*_handlers.go`,
`handlers.go`, `peer_settings.go`, `update_handlers.go`) is protected only by
`http.NewCrossOriginProtection`. Anyone who can reach the Agent can create
invitations, manage local users, change settings and trigger updates.
Depends on D2.

Task: write `docs/node-operator-authentication.md`. It covers:

- operator identity, storage and credential types per D2;
- first-operator bootstrap without a default password, for example a
  one-time setup code printed by `install.sh` or `arknet-node`;
- sessions, cookie flags, CSRF and session expiry;
- an endpoint-by-permission matrix covering every current route, including
  which routes stay unauthenticated (health, and member-facing login);
- recovery when the operator loses access, using local shell access only;
- how this differs from global and local *user* sessions, which must not
  gain operator rights.

Acceptance criteria:

- The matrix lists every route registered in `node-agent`. Check this with a
  script, not by hand.
- The document lists open questions for the user. No code changes.

## Prompt 45A — Operator Accounts, Bootstrap and Sessions

Implement the identity, bootstrap, session and recovery parts of the 45
design in the Agent, PostgreSQL migrations and `arknet-node`. Do not protect
routes yet.

Acceptance criteria:

- Tests cover bootstrap single-use, credential verification, session
  expiry, logout, recovery and migration on an existing Node database.
- The bootstrap secret never appears in logs. A log-capture test checks this.

## Prompt 45B — Enforce the Permission Matrix

Task:

1. Require an operator session on every route the matrix marks as operator.
   Deny before any side effect.
2. Add a table-driven test generated from the registered routes. It fails if
   a new route has no matrix entry.
3. Add operator login and logout to `node-ui`. No operator data in browser
   storage beyond the session cookie.
4. Update `install.sh`, the upgrade path and the docs so existing Nodes get
   a bootstrap code on upgrade.
5. Ship the self-signed HTTPS management listener from the design's
   "LAN HTTPS listener" section. `install.sh` and the upgrade path generate the
   Node's certificate and print its SHA-256 fingerprint next to the setup code.

Acceptance criteria:

- Anonymous, local-user and global-member callers get 401 or 403 on every
  operator route, and repositories are not called (spy test).
- The full E2E and upgrade checks pass with the harness logging in as an
  operator. Item 20 gains a check that an unauthenticated caller on Node B's
  management address is denied.
- A browser-level test signs in over the HTTPS listener and confirms that the
  plain-HTTP listener is reachable only on loopback.
- Close ARKSEC-002 in `docs/security-follow-ups.md`, citing the run.

## Prompt 45C — Separate Member Listener

Context: decision Q6 in `docs/node-operator-authentication.md`. Member login
and session routes share the Agent listener with the management plane.

Task: serve the `public` and `member` routes from the permission matrix on
their own listener and port, and keep the management listener loopback-only
except for the HTTPS listener from 45B. Update compose, `install.sh`, the
upgrade path, the live-test ingress and the docs.

Acceptance criteria:

- Every operator route answers 404 on the member listener, and every member
  route answers 404 on the management listener, using a test generated from
  the registered routes.
- The full E2E and upgrade checks pass.

---

# PHASE 20C — Global Credentials (ARKSEC-001)

## Prompt 46 — Design: Core-Bound Login Without Password Transit

Context: `POST /api/v1/global-users/sessions` sends the reusable global
password to the Node. The Node then keeps a password-derived offline
verifier (`node-agent/internal/globalauth`). Core already issues offline
authorizations (`/api/federation/v1/nodes/{node_id}/offline-authorizations`).
Depends on D3 and D4.

Task: write `docs/global-login-assertions.md`. It covers:

- the online flow: the user authenticates at Core. Core issues a one-time
  assertion bound to Node, member ID, purpose, nonce and expiry. The Node
  verifies it with Core's published key and creates a Node session.
- the offline model per D3. Compare at least two options, for example a
  Core-signed, Node-bound offline grant plus a per-Node credential the user
  holds, or a device-bound passkey. State what a malicious Node can and
  cannot do with each.
- replay, cross-Node, expiry and clock-skew handling;
- migration: existing Node verifiers are deleted, and affected users simply
  log in online once;
- what happens to pairwise IDs and privacy (no new correlatable value).

Acceptance criteria:

- A threat table answers: can a malicious Node capture anything usable at
  Core or another Node? The answer must be "no" for the chosen design.
- Open questions are listed for the user. No code changes.

## Prompt 46A — Core Assertion Issuance

Implement the Core side: login page or endpoint, assertion signing key
(separate from the tracker key), issuance, single-use nonce storage, and the
published verification key.

Acceptance criteria:

- Tests cover binding, expiry, single use, wrong Node, wrong member and key
  rotation.
- No password or assertion appears in Core logs (log-capture test).

## Prompt 46B — Agent Accepts Assertions; Password Path Removed

Task:

1. The Agent accepts assertions, verifies them and creates Node sessions.
2. Remove password submission to the Node and the Node-held verifier.
   Migrate existing databases by deleting stored verifiers.
3. Implement the offline model chosen in 46.
4. Update `node-ui` login to use the Core redirect.

Acceptance criteria:

- An E2E step proves that a "malicious Node" (a harness that records every
  byte the Agent receives during login) captures nothing that logs in at
  Core or at Node B.
- Item 13 is rewritten to the new model and passes: online login, offline
  login within the window, rejection after the window, replay rejection, and
  cross-Node rejection.
- Close ARKSEC-001, citing the run. Update `docs/global-users.md` and
  `docs/cached-global-authorization.md`.

---

# PHASE 20D — Revocation (ARKSEC-006)

## Prompt 47 — Terminate Active Transfers on Revocation

Context: `node-agent/internal/heartbeat/worker.go` moves a confirmed
`REVOKED/BLOCKED` Node to terminal `REVOKED`. The seed reconciler unseeds
non-CONNECTED Nodes, but active downloads continue.

Task:

1. On terminal `REVOKED`, the Agent cancels every active transfer and seed,
   keeps verified local media, and refuses new transfer actions.
2. The tracker drops a revoked Node's peers no later than the next announce.
   Document the bounded residual window for a malicious Node that ignores
   revocation; enforcing that is ARKSEC-004's job.
3. Add an E2E step: start a transfer on Node B, revoke Node B, and measure
   that its byte counters stop within a stated bound.

Acceptance criteria:

- Unit tests for cancellation on revocation, and the E2E byte-counter step
  passes.
- Close ARKSEC-006 for cooperative Nodes. Record the remaining malicious-Node
  window as part of ARKSEC-004.

---

# PHASE 20E — Private Relay (ARKSEC-004)

## Prompt 48 — Design Spike: Default Private Relay

Context: Node peers connect directly, so each learns the other's origin
address. Depends on D5.

Task:

1. Write `docs/private-relay-design.md`. Compare at least: a Core-operated
   relay, volunteer relay Nodes, and an existing transport such as a
   TURN-style relay or a WireGuard mesh. Cover authorization (Core-issued,
   swarm-scoped), metadata minimization, bandwidth cost, fail-closed
   behavior, and how revocation cuts sessions.
2. Build a throwaway proof of concept for the leading option in
   `scripts/spikes/`. Measure throughput and confirm that neither peer
   learns the other's address.

Acceptance criteria:

- The document recommends one option, gives measured numbers, and lists
  open questions for the user.
- No product code changes. Implementation prompts are written after the user
  accepts the design.

Outcome: accepted 2026-11-03 (D5). Implementation prompts follow.

## Prompt 48A — Relay Service

Context: [private relay design](../private-relay-design.md), option A. The
spike in `scripts/spikes/private-relay/` is a reference only; do not copy it
without tests.

Task:

1. Add a `relay/` Go module (standard library only, like `tracker/`):
   - TLS 1.3 listener; a seeder registers with a Core-signed **seed** grant
     (existing tracker grant format, verified with Core's public key) and
     gets a random virtual endpoint in `100.64.0.0/10`;
   - a downloader connects to an endpoint with a **download** grant for the
     same info hash; the relay splices the two streams;
   - sessions and registrations end at grant expiry unless renewed on the
     control channel with a fresh grant;
   - bounded header sizes, connection counts and accept timeouts;
   - no persistence; logs carry no addresses, grants, hashes, subjects or
     endpoints (same rule as the tracker).
2. A Core-authenticated revocation endpoint: Core sends a signed list of
   pairwise subjects; the relay closes their registrations and sessions at
   once and refuses them until their grants could have expired.
3. Container image, Compose overlay under `deploy/core/`, README.

Acceptance criteria:

- Unit tests: grant verification (action, hash binding, expiry, signature),
  endpoint routing, wrong-hash and wrong-action refusal, expiry closes live
  sessions, renewal extends them, signed revocation closes them and refuses
  reconnects, unsigned or replayed revocations are rejected, limits enforced.
- A log check proves no address, grant or subject is logged.
- Mutation-check the authorization and revocation guards.

## Prompt 48B — Core: Relay List and Revocation Push

Task:

1. Core configuration lists the operator relays (URL plus pinned
   certificate or key). Core serves a **signed relay list** on the
   federation API; Nodes fetch it with their heartbeat refresh.
2. On revocation (and on BLOCKED), Core computes the revoked Node's pairwise
   subjects for every catalog object and pushes a signed revocation to every
   relay, retrying until each acknowledges. Failures are visible to the
   admin and in logs (without subjects).
3. A per-relay signing key for revocations, distinct from the tracker and
   assertion keys.

Acceptance criteria:

- Tests: relay list signature and content; revocation reaches every relay;
  retry after a relay outage; unrelated Nodes unaffected; keys distinct.

## Prompt 48C — Tracker Advertises Relay Endpoints Only

Context: decision 4 keeps the tracker separate. Proposed mechanism, to be
confirmed by tests: the relay returns a **relay-signed endpoint token**
(endpoint, subject, info hash, expiry) at registration; the seeder includes
it in its announce; the tracker verifies it with the relay key and
advertises that endpoint.

Task:

1. Complete peers are advertised only as verified relay endpoints, never as
   the announce `RemoteAddr`. Announces without a valid endpoint token are
   accepted for downloaders only and never advertised.
2. The tracker stores no origin addresses.

Acceptance criteria:

- Tests: origins never appear in responses (IPv4 and IPv6); forged, expired
  or other-hash endpoint tokens rejected; existing tracker tests updated.

## Prompt 48D — Node Connector, Fail-Closed Transfers, and E2E

Task:

1. A connector in the transfer container's network namespace (or inside the
   transfer service): SOCKS5 side for libtorrent's outbound peers, listen
   side dialing libtorrent on `127.0.0.1`. Grants come from the Agent.
2. libtorrent: `proxy_type` SOCKS5 with `proxy_peer_connections`, uTP off,
   `allow_multiple_connections_per_ip`, DHT/LSD/PEX off (already).
3. The transfer container can reach only the connector, tracker and web seed
   (Compose network design plus a check). No direct peer path exists.
4. Fail closed: no relay reachable stops peer transfers; web seed continues.

Acceptance criteria (ARKSEC-004 closure):

- MVP E2E item 11 (A→B contribution) passes through the relay, and a capture
  proves neither Node's Agent or transfer container received the other's
  address (text and binary form), including BEP 10 `yourip`.
- With every relay stopped, peer transfer stops and no direct connection is
  attempted (packet or connection-level check).
- Item 16 extended: revoking Node B closes its relayed peer session within a
  stated bound (Core push), not only its web-seed transfer.
- Close ARKSEC-004, citing the run. Update `docs/node-revocation.md`: the
  open-peer-connection row of the malicious-Node window becomes bounded.

## Prompt 48E — Design Spike: Community-Run Relays

Context: decision 1 allows community relays only under the
[conditions](../private-relay-design.md#conditions-for-community-run-relays)
in the design. Depends on 48A–48D.

Task:

1. Design the two-hop path (community hop + operator hop), end-to-end
   encryption to the operator relay, Core admission and revocation of
   community relays.
2. Throwaway spike in `scripts/spikes/`: prove at each hop which addresses
   are visible, measure throughput, and measure the operator hop's egress
   saving.

Acceptance criteria:

- The document shows no community relay sees both Nodes' addresses, grants
  or info hashes, gives measured numbers, and lists open questions.
- No product code changes.

---

# PHASE 20F — Remaining Medium Items

## Prompt 49 — Scoped Core Capabilities (ARKSEC-005)

Replace the shared development admin bearer with administrator identities and
route-scoped permissions: node decisions, user administration, media
administration, membership actions and swarm grants. Split Node key authority
by capability where it reduces blast radius.

Acceptance criteria: denied calls never reach repositories (spy test);
administrative actions are attributable in an audit log; the E2E and
live-test scripts use scoped credentials.

## Prompt 49A — Login and Enrollment Abuse Controls (ARKSEC-008)

Add bounded rate limits to identity login and enrollment, pending-enrollment
quotas and expiry, and make signed enrollment mandatory, removing the
unsigned compatibility path.

Acceptance criteria: limits, quotas and expiry are tested; unknown and
decided Node IDs return indistinguishable responses; the E2E still passes.

## Prompt 49B — Supported TLS Ingress With URL Redaction (ARKSEC-007)

Ship a supported reverse-proxy configuration for Core and the tracker that
redacts grant-bearing path segments from access logs.

Acceptance criteria: a sentinel grant sent through the supported ingress is
absent from proxy, Core, tracker, Agent and UI logs (automated check).

---

# PHASE 20G — Validation

## Prompt 50 — Phase 20 Validation Report

Task:

1. On one frozen, committed tree, rerun: the full E2E, fresh install,
   upgrade, product swarm, all component tests, and every ARKSEC acceptance
   test added in this phase.
2. Update `docs/mvp-validation-report.md` and `docs/security-follow-ups.md`.
3. Update the live-test group to the same tree and run its verifiers.
4. Give a verdict: is ArkNet ready for hostile multi-operator production? If
   ARKSEC-004 is still open, the answer is no, and the report says what the
   remaining risk is.

Acceptance criteria:

- Every PASS cites a log from the frozen tree, identified by commit hash.
- Every ARKSEC item is either closed with cited evidence or explicitly open
  with a reason.
