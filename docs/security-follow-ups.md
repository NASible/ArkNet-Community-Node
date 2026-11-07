# Security follow-ups

This is the repository tracker for security work identified by the MVP review. An
item remains open until its acceptance criteria are covered by automated tests.

| ID | Priority | Status | Follow-up | Acceptance criteria |
| --- | --- | --- | --- | --- |
| <a id="arksec-001"></a>ARKSEC-001 | Critical | **Closed** 2026-10-31 (Prompts 46A/46B; MVP E2E run `arknet-p43-test-0737adcda8fa`, item 13 and its malicious-Node harness; see [validation](development/prompt46b-validation.md)) Re-verified on frozen commit `b232f46` (Prompt 50, E item 13). See [Phase 20 validation](mvp-validation-report.md#arksec-status-on-the-frozen-tree). | Remove reusable global passwords from the community-Node trust boundary. Replace Node-mediated password submission and Node-held password-derived verifiers with a Core-authenticated, one-time, Node-bound assertion or a reviewed PAKE/passkey design. | A malicious Node cannot capture a credential usable at Core or another Node; assertions bind Node, member, purpose, nonce, and expiry; replay and cross-Node tests pass. |
| <a id="arksec-002"></a>ARKSEC-002 | High | Closed (2026-10-28 AKDT, Prompt 45B) Re-verified on frozen commit `b232f46` (Prompt 50: E item 20, operator HTTPS check, upgrade). See [Phase 20 validation](mvp-validation-report.md#arksec-status-on-the-frozen-tree). | Node-local operator sessions now guard every management route before handler side effects. Generated route/matrix and repository-spy tests pass. The operator-authenticated MVP E2E (including Node B denial), upgrade/bootstrap/fingerprint check and real Chromium LAN HTTPS login/logout check passed; see [Prompt 45B validation](development/prompt45b-validation.md). | Anonymous and non-admin callers are denied before side effects; an endpoint/permission matrix is tested; invitation secrets are returned only to an authorized operator. |
| <a id="arksec-003"></a>ARKSEC-003 | High | Fixed (live proof 2026-10-27) Re-verified on frozen commit `b232f46` over the private relay (Prompt 50, product swarm: OFF 1,228 B, ON 4,119,854 B, after OFF 0 B). See [Phase 20 validation](mvp-validation-report.md#arksec-status-on-the-frozen-tree). | Enforce Peer Serving OFF in the transfer runtime, including while a torrent is incomplete. The tracker never advertises incomplete peers; the Agent seed reconciler serves only with Peer Serving ON and a fresh Core seed grant, and unseeds within one interval when it turns OFF or authorization ends (`docs/peer-serving.md`). Proven by `scripts/check-multi-node-swarm-stack.py` (`EXIT 0`) on a multi-piece object: Node A sent 2,900 bytes in total while OFF (less than one 16 KiB block), 3,725,555 bytes while ON, and 0 bytes after OFF; see [MVP validation report](mvp-validation-report.md). | A real multi-piece libtorrent test records zero payload upload while disabled; enabling permits upload only with fresh seed authorization; disabling an active session stops upload. |
| <a id="arksec-004"></a>ARKSEC-004 | High | **Closed** 2026-11-04 (Prompt 48D; MVP E2E run `arknet-p43-test-795aafddced2`, 17/17, items 11 and 16; see [validation](development/prompt48d-validation.md)). Built on the [private relay design](private-relay-design.md) (48), [relay](../relay/README.md) (48A), Core relay list and revocation push (48B) and [endpoint tokens](relay-endpoint-tokens.md) (48C). Residual malicious-Node grant windows are in [Node revocation](node-revocation.md#malicious-node-residual-window-arksec-004) Re-verified on frozen commit `b232f46` (Prompt 50: E items 11 and 16, relay-only product swarm). See [Phase 20 validation](mvp-validation-report.md#arksec-status-on-the-frozen-tree). | Make private relay the default peer transport and direct peer addressing an explicit opt-in fallback. | With default configuration, peers neither receive each other's origin address nor exchange payload directly; relay failure does not silently enable direct mode. |
| <a id="arksec-005"></a>ARKSEC-005 | Medium | **Closed** 2026-11-06 (Prompt 49; MVP E2E run `arknet-p43-test-68e413875194`, 17/17; route/permission spy tests, real-Postgres audit and revocation checks; see [validation](development/prompt49-validation.md)) Re-verified on frozen commit `b232f46` (Prompt 50: Core admin stack, component spy/audit tests; E2E and live-test scripts mint scoped tokens). See [Phase 20 validation](mvp-validation-report.md#arksec-status-on-the-frozen-tree). | Replace the shared Core development admin bearer and broad Node key authority with identities and route-scoped capabilities. | Separate permissions cover node decisions, user administration, media administration, membership actions, and swarm grants; denied calls do not reach repositories; administrative actions are attributable and auditable. |
| <a id="arksec-006"></a>ARKSEC-006 | Medium | **Closed for cooperative Nodes** 2026-10-31 (Prompt 47; MVP E2E run `arknet-p43-test-e5cc73079748`, item 16; negative control `arknet-p43-test-5542cab09874`; see [validation](development/prompt47-validation.md)). Malicious-Node enforcement moved to ARKSEC-004 Re-verified on frozen commit `b232f46` (Prompt 50, E item 16). See [Phase 20 validation](mvp-validation-report.md#arksec-status-on-the-frozen-tree). | Define and enforce active-session revocation. Cooperative Agents must stop transfers on authoritative `REVOKED`/`BLOCKED`; malicious-node enforcement requires relay/session controls. | Revocation denies new grants, cancels cooperative active handles, stops byte counters, and bounds any residual authorization window in a documented integration test. |
| <a id="arksec-007"></a>ARKSEC-007 | Medium | **Closed for the supported ingress recipe** (Prompt 49B; [validation](development/prompt49b-validation.md)): real verified-TLS sentinel check passed across proxy/Core/tracker/Agent/UI raw logs, including forced failures and unsafe-logging negative control; component gate 13/13. External proxies and unsupported debug logging remain operator responsibilities. Re-verified on frozen commit `b232f46` (Prompt 50, gate step `ingress-redaction`). See [Phase 20 validation](mvp-validation-report.md#arksec-status-on-the-frozen-tree). | Provide a supported TLS ingress configuration that redacts tracker, torrent, and web-seed bearer URLs. | A sentinel grant traverses the supported ingress and is absent from proxy, Core, tracker, Agent, and UI logs. |
| <a id="arksec-008"></a>ARKSEC-008 | Medium | **Closed for the scoped acceptance criteria** (Prompt 49A; [validation](development/prompt49a-validation.md)): limits, atomic quota/expiry, mandatory signed enrollment and enumeration-safe receipts tested; MVP E2E passed. Full gate remains 15/16 due to a separate product-swarm relay-configuration gap. Re-verified on frozen commit `b232f46` (Prompt 50: enrollment stack, component tests, E2E under production limits). The product-swarm gap noted at 49A is fixed (`c34b664`). See [Phase 20 validation](mvp-validation-report.md#arksec-status-on-the-frozen-tree). | Add abuse controls to identity login and enrollment; retire the unsigned enrollment compatibility path. | Bounded rate limits and pending-enrollment quotas/expiry are tested; signed enrollment is mandatory; unknown and decided Node IDs do not form a useful unauthenticated oracle. |
| <a id="arksec-009"></a>ARKSEC-009 | Medium | **Closed** 2026-11-06 (Prompt 50 follow-up): the operator confirmed that the credentials from the removed local `.env` files and generated test artifacts were throwaway. Live-test application secrets were rotated and verified in Prompt 44B (`deploy/live-test/rotate-credentials.py`, 19 checks, `EXIT 0`). The live tracker, assertion and relay keys were generated on the VMs and never came from a local `.env`. Prompt 49 removed the shared development admin bearer from the product; Core refuses to start with it configured (`check-core-admin-stack.py` on frozen commit `b232f46`). See [Phase 20 validation](mvp-validation-report.md#arksec-status-on-the-frozen-tree). | Rotate credentials that may have been used from removed local `.env` and generated test artifacts. | Operators confirm affected database passwords, development admin token, transfer token, and tracker signing key are rotated or were disposable; no removed value remains active. |

## Phase 19A evidence status (2026-10-27 AKDT)

The [MVP validation report](mvp-validation-report.md) records 20/20 Prompt 43
items passing on the final tree. Prompts 43C–43E implement the ARKSEC-003
worker/API/reconciler/tracker controls, and Prompt 43F removes the test-only
seeder. After the worker's web-seed churn and resume-metadata defects were fixed,
the product-path swarm check passed with the OFF/ON/OFF measurement in the table
above. ARKSEC-003 is **closed**.

ARKSEC-006 remains open: the Agent stops cooperative seeds when not CONNECTED,
including after revocation, but active downloads are not forcibly terminated.
Denying new grants or passing the functional revocation step is insufficient to
close its active-session/byte-counter acceptance criteria. At that Phase 19A
snapshot, ARKSEC-001 and ARKSEC-002 were unchanged production blockers.

## Phase 20B management-plane closure (2026-10-28 AKDT)

Prompt 45B closes **ARKSEC-002** in the source implementation, backed by the
[validation run](development/prompt45b-validation.md). Anonymous/member callers
cannot enter operator handlers; route coverage is generated from registration,
and invitation creation requires recent operator authentication. Installer and
upgrade generate the Node certificate and print its fingerprint beside the
bootstrap code. Chromium verifies LAN HTTPS sign-in/logout and loopback-only
plain HTTP. Existing deployments must install the new release; the separate
live-test VMs were not redeployed by this prompt. ARKSEC-001 and ARKSEC-004
remain release blockers.

## Phase 20C global-credential closure (2026-10-31 AKDT)

Prompts 46A and 46B close **ARKSEC-001**. Global members sign in only on Core's
own page; Core issues a one-time Ed25519 assertion bound to the Node, the
pairwise member ID, the purpose, a Node nonce and a browser-held challenge. The
Agent verifies it with Core's published key set. Nodes no longer receive or
store global passwords or password-derived verifiers: migration 0012 deleted
existing verifiers, and both password routes (Node login and Core's
Node-relayed check) answer `410`. Offline login uses the Core-signed grant plus
a per-Node device credential, bounded by D3 (30 days). MVP E2E run
`arknet-p43-test-0737adcda8fa` recorded every byte Node A's Agent received
during a login. Nothing it captured logged in at Core or at Node B. Replay,
cross-Node, offline-window and after-window checks passed. See the
[validation note](development/prompt46b-validation.md). The live-test VMs were
not redeployed. ARKSEC-004 remains a release blocker.

## Phase 20D revocation closure (2026-10-31 AKDT)

Prompt 47 closes **ARKSEC-006 for cooperative Nodes**. On terminal `REVOKED`
the Agent wakes its reconcilers at once, cancels every active transfer and
seed, keeps verified local media, and refuses new transfers with
`409 node_revoked`. MVP E2E run `arknet-p43-test-e5cc73079748` revoked Node B
while it was downloading from Core's web seed: Node B entered `REVOKED` 3.8 s
later, received 3,757 bytes in the next 52 s (bound: no block after heartbeat
interval + 10 s), stopped at 10.6 of 39.2 MB, and left the tracker swarm. The
same run with the Agent's cancellation disabled (`arknet-p43-test-5542cab09874`)
failed: blocks were still arriving 45.2 s after revocation. See the
[validation note](development/prompt47-validation.md).

A Node that ignores revocation keeps, from Core's decision: its tracker and
web-seed grants for at most 5 minutes; an open web-seed response for at most
30 s past that (Core's write timeout); and open or remembered peer connections
to seeding Nodes without a time bound. That window is recorded under
**ARKSEC-004** and documented in [Node revocation](node-revocation.md).

## Phase 20E relay closure (2026-11-04 AKST)

Prompt 48D closes **ARKSEC-004**. Peer transport is relay-only by default: the
transfer worker dials peers only through its in-namespace connector (SOCKS5
limited to the virtual `100.64.0.0/10` range), which reaches peers over pinned
TLS 1.3 to operator relays from Core's signed relay list. Trackers advertise
only relay-signed virtual endpoints. There is no direct mode and no fallback: a
root entrypoint installs a default-deny namespace firewall (tracker, web seed,
configured relays and loopback only) and then drops to UID 10001 with no
capabilities. DHT, LSD, PEX and uTP are off.

MVP E2E run `arknet-p43-test-795aafddced2` passed 17/17:

- Item 11: Node A seeded 33.58 MB to Node B only through the relay; B verified
  the SHA-256 with its web seed disconnected. Full-namespace captures of both
  Agents and both transfer containers held none of the counterpart's addresses
  (text, packed binary, BEP 10 `yourip`), every outgoing attempt went to an
  approved gateway, and the firewall recorded 0 denied attempts. With every
  relay stopped, peer bytes stopped at once and stayed at 0 for 53 s while 76 MB
  still arrived from Core's web seed.
- Item 16: with B's Agent frozen (so only Core enforcement could act), Core's
  revocation push reached the relay 1.74 s after the decision and closed B's
  live relayed session; B's received bytes stopped 1.36 s after revocation and
  stayed quiet for 51 s.

What a revoked malicious Node keeps is now bounded by grant lifetime; see
[Node revocation](node-revocation.md#malicious-node-residual-window-arksec-004).
The live-test VMs were not redeployed.

The MVP security review is in [MVP security review](mvp-security-review.md).
