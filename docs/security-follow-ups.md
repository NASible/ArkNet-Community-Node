# Security follow-ups

This is the repository tracker for security work identified by the MVP review. An
item remains open until its acceptance criteria are covered by automated tests.

| ID | Priority | Status | Follow-up | Acceptance criteria |
| --- | --- | --- | --- | --- |
| <a id="arksec-001"></a>ARKSEC-001 | Critical | **Closed** 2026-10-31 (Prompts 46A/46B; MVP E2E run `arknet-p43-test-0737adcda8fa`, item 13 and its malicious-Node harness; see [validation](development/prompt46b-validation.md)) | Remove reusable global passwords from the community-Node trust boundary. Replace Node-mediated password submission and Node-held password-derived verifiers with a Core-authenticated, one-time, Node-bound assertion or a reviewed PAKE/passkey design. | A malicious Node cannot capture a credential usable at Core or another Node; assertions bind Node, member, purpose, nonce, and expiry; replay and cross-Node tests pass. |
| <a id="arksec-002"></a>ARKSEC-002 | High | Closed (2026-10-28 AKDT, Prompt 45B) | Node-local operator sessions now guard every management route before handler side effects. Generated route/matrix and repository-spy tests pass. The operator-authenticated MVP E2E (including Node B denial), upgrade/bootstrap/fingerprint check and real Chromium LAN HTTPS login/logout check passed; see [Prompt 45B validation](development/prompt45b-validation.md). | Anonymous and non-admin callers are denied before side effects; an endpoint/permission matrix is tested; invitation secrets are returned only to an authorized operator. |
| <a id="arksec-003"></a>ARKSEC-003 | High | Fixed (live proof 2026-10-27) | Enforce Peer Serving OFF in the transfer runtime, including while a torrent is incomplete. The tracker never advertises incomplete peers; the Agent seed reconciler serves only with Peer Serving ON and a fresh Core seed grant, and unseeds within one interval when it turns OFF or authorization ends (`docs/peer-serving.md`). Proven by `scripts/check-multi-node-swarm-stack.py` (`EXIT 0`) on a multi-piece object: Node A sent 2,900 bytes in total while OFF (less than one 16 KiB block), 3,725,555 bytes while ON, and 0 bytes after OFF; see [MVP validation report](mvp-validation-report.md). | A real multi-piece libtorrent test records zero payload upload while disabled; enabling permits upload only with fresh seed authorization; disabling an active session stops upload. |
| <a id="arksec-004"></a>ARKSEC-004 | High | Open — design accepted 2026-11-03 (Prompt 48, [private relay design](private-relay-design.md)); relay service built (Prompt 48A, [relay](../relay/README.md)); Core relay list and revocation push built (Prompt 48B); tracker endpoints and Node connector pending (48C–48D); also owns the malicious-Node revocation window recorded by Prompt 47 ([Node revocation](node-revocation.md#malicious-node-residual-window-arksec-004)) | Make private relay the default peer transport and direct peer addressing an explicit opt-in fallback. | With default configuration, peers neither receive each other's origin address nor exchange payload directly; relay failure does not silently enable direct mode. |
| <a id="arksec-005"></a>ARKSEC-005 | Medium | Open | Replace the shared Core development admin bearer and broad Node key authority with identities and route-scoped capabilities. | Separate permissions cover node decisions, user administration, media administration, membership actions, and swarm grants; denied calls do not reach repositories; administrative actions are attributable and auditable. |
| <a id="arksec-006"></a>ARKSEC-006 | Medium | **Closed for cooperative Nodes** 2026-10-31 (Prompt 47; MVP E2E run `arknet-p43-test-e5cc73079748`, item 16; negative control `arknet-p43-test-5542cab09874`; see [validation](development/prompt47-validation.md)). Malicious-Node enforcement moved to ARKSEC-004 | Define and enforce active-session revocation. Cooperative Agents must stop transfers on authoritative `REVOKED`/`BLOCKED`; malicious-node enforcement requires relay/session controls. | Revocation denies new grants, cancels cooperative active handles, stops byte counters, and bounds any residual authorization window in a documented integration test. |
| <a id="arksec-007"></a>ARKSEC-007 | Medium | Open | Provide a supported TLS ingress configuration that redacts tracker, torrent, and web-seed bearer URLs. | A sentinel grant traverses the supported ingress and is absent from proxy, Core, tracker, Agent, and UI logs. |
| <a id="arksec-008"></a>ARKSEC-008 | Medium | Open | Add abuse controls to identity login and enrollment; retire the unsigned enrollment compatibility path. | Bounded rate limits and pending-enrollment quotas/expiry are tested; signed enrollment is mandatory; unknown and decided Node IDs do not form a useful unauthenticated oracle. |
| <a id="arksec-009"></a>ARKSEC-009 | Medium | Open — **addressed for the live-test group only** (2026-10-27, Prompt 44B): `deploy/live-test/rotate-credentials.py` rotated the global-user, Jellyfin administrator, Core admin token, PostgreSQL and transfer-token secrets and proved every new value accepted and every old one rejected (19 checks, `EXIT 0`; `verify-deployment.py` `EXIT 0` afterwards). The Core admin token was also removed from Node VMs. Other environments, removed local `.env` values and the tracker signing key are not covered. | Rotate credentials that may have been used from removed local `.env` and generated test artifacts. | Operators confirm affected database passwords, development admin token, transfer token, and tracker signing key are rotated or were disposable; no removed value remains active. |

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

The MVP security review is in [MVP security review](mvp-security-review.md).
