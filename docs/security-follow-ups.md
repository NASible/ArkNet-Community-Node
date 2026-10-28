# Security follow-ups

This is the repository tracker for security work identified by the MVP review. An
item remains open until its acceptance criteria are covered by automated tests.

| ID | Priority | Status | Follow-up | Acceptance criteria |
| --- | --- | --- | --- | --- |
| <a id="arksec-001"></a>ARKSEC-001 | Critical | Open | Remove reusable global passwords from the community-Node trust boundary. Replace Node-mediated password submission and Node-held password-derived verifiers with a Core-authenticated, one-time, Node-bound assertion or a reviewed PAKE/passkey design. | A malicious Node cannot capture a credential usable at Core or another Node; assertions bind Node, member, purpose, nonce, and expiry; replay and cross-Node tests pass. |
| <a id="arksec-002"></a>ARKSEC-002 | High | Open | Authenticate and authorize the Node management plane, especially invitation creation, local-user administration, settings, updates, and transfer controls. | Anonymous and non-admin callers are denied before side effects; an endpoint/permission matrix is tested; invitation secrets are returned only to an authorized operator. |
| <a id="arksec-003"></a>ARKSEC-003 | High | Fixed (live proof 2026-10-27) | Enforce Peer Serving OFF in the transfer runtime, including while a torrent is incomplete. The tracker never advertises incomplete peers; the Agent seed reconciler serves only with Peer Serving ON and a fresh Core seed grant, and unseeds within one interval when it turns OFF or authorization ends (`docs/peer-serving.md`). Proven by `scripts/check-multi-node-swarm-stack.py` (`EXIT 0`) on a multi-piece object: Node A sent 2,900 bytes in total while OFF (less than one 16 KiB block), 3,725,555 bytes while ON, and 0 bytes after OFF; see [MVP validation report](mvp-validation-report.md). | A real multi-piece libtorrent test records zero payload upload while disabled; enabling permits upload only with fresh seed authorization; disabling an active session stops upload. |
| <a id="arksec-004"></a>ARKSEC-004 | High | Open | Make private relay the default peer transport and direct peer addressing an explicit opt-in fallback. | With default configuration, peers neither receive each other's origin address nor exchange payload directly; relay failure does not silently enable direct mode. |
| <a id="arksec-005"></a>ARKSEC-005 | Medium | Open | Replace the shared Core development admin bearer and broad Node key authority with identities and route-scoped capabilities. | Separate permissions cover node decisions, user administration, media administration, membership actions, and swarm grants; denied calls do not reach repositories; administrative actions are attributable and auditable. |
| <a id="arksec-006"></a>ARKSEC-006 | Medium | Open | Define and enforce active-session revocation. Cooperative Agents must stop transfers on authoritative `REVOKED`/`BLOCKED`; malicious-node enforcement requires relay/session controls. | Revocation denies new grants, cancels cooperative active handles, stops byte counters, and bounds any residual authorization window in a documented integration test. |
| <a id="arksec-007"></a>ARKSEC-007 | Medium | Open | Provide a supported TLS ingress configuration that redacts tracker, torrent, and web-seed bearer URLs. | A sentinel grant traverses the supported ingress and is absent from proxy, Core, tracker, Agent, and UI logs. |
| <a id="arksec-008"></a>ARKSEC-008 | Medium | Open | Add abuse controls to identity login and enrollment; retire the unsigned enrollment compatibility path. | Bounded rate limits and pending-enrollment quotas/expiry are tested; signed enrollment is mandatory; unknown and decided Node IDs do not form a useful unauthenticated oracle. |
| <a id="arksec-009"></a>ARKSEC-009 | Medium | Open | Rotate credentials that may have been used from removed local `.env` and generated test artifacts. | Operators confirm affected database passwords, development admin token, transfer token, and tracker signing key are rotated or were disposable; no removed value remains active. |

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
close its active-session/byte-counter acceptance criteria. ARKSEC-001 and
ARKSEC-002 are unchanged and remain production blockers.

The MVP security review is in [MVP security review](mvp-security-review.md).
