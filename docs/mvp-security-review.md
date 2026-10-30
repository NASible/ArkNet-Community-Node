# ArkNet MVP security review

## Verdict

The reviewed MVP has strong cryptographic Node identity, Node-scoped federation
routes, authorization-gated private swarms, and disabled public peer-discovery
mechanisms. It is **not ready for hostile multi-operator production use** until
the critical global-credential boundary and high-priority management-plane and
relay follow-ups are resolved. The peer-serving follow-up (ARKSEC-003) is closed.

This review covered source and tests in `core/`, `node-agent/`, `node-ui/`,
`tracker/`, `transfer/`, and both Compose deployments. The supplied snapshot had
no `.git` metadata, so repository history and whether removed local artifacts
were ever published could not be established.

## Phase 19A status update (2026-10-27 AKDT)

See the [MVP validation report](mvp-validation-report.md) for the 20-item E2E
matrix and log provenance. All 20 items pass on the final tree, so the MVP is
functionally complete. That result must not be used in place of this security
review.

Prompts 43C–43E added runtime-only, upload-only seed handles; full local hash
verification; bearer-protected transfer seed routes; strict seed-only Core grants;
and Agent reconciliation of consent, local availability, Node state, and grant
lifetime. The tracker advertises only complete, seed-authorized peers.

Prompt 43F removed the test-only seeder from the harness. On the final tree, the
product-path swarm check passed (`EXIT 0`) with a live OFF/ON/OFF measurement.
Node A transmitted 2,900 bytes in total while Peer Serving was OFF, which is less
than one 16 KiB block. It uploaded 3,725,555 bytes while ON, and transmitted
0 bytes after Peer Serving was turned off mid-session. **ARKSEC-003 is closed.**

ARKSEC-006 remains **open**: cooperative seeds are unseeded when the Node is not
CONNECTED, but active downloads are not forcibly cancelled and established
malicious-peer sessions are not immediately revoked. ARKSEC-001 and ARKSEC-004
remain open despite passing functional authentication, isolation and swarm checks.
ARKSEC-002 was subsequently closed by Prompt 45B's generated permission/spies,
operator-authenticated E2E/upgrade and real browser HTTPS proof; see
[Prompt 45B validation](development/prompt45b-validation.md).

## Requirement results

| Requirement | Result | Evidence and disposition |
| --- | --- | --- |
| Nodes cannot unnecessarily enumerate community Nodes | Pass | Node federation routes are self-scoped and no Node directory is exposed. Tracker discovery is limited to authorized peers in one swarm. |
| Operators cannot enumerate global users | Fixed | Removed the Core Node-membership list, Agent `/api/v1/global-members`, and UI global-member panel. Tests require the former routes to return `404` and the UI to make no listing request. Invitation acceptance and per-member authorization remain. |
| Local users never synchronize | Pass | Local-user storage and lifecycle remain Node-local. Tests record outbound Core requests and reject local identifiers, usernames, passwords, or sessions in them. |
| Pairwise membership IDs are not trivially correlatable | Partial | IDs are independent 256-bit random values and Core alone stores their mapping. The current global-login flow still exposes the reusable global password to an untrusted Node, creating a simpler correlation and credential-theft channel (`ARKSEC-001`). |
| Node credentials are unique | Pass | Nodes generate local Ed25519 credentials; Core binds one public key per Node and enforces public-key uniqueness. |
| Tracker requires swarm authorization | Pass | `/announce/{grant}` verifies signature, expiry, v2 hash, subject, and required download/seed action before swarm mutation. |
| DHT, PEX, and LSD remain disabled | Pass | Libtorrent session and per-torrent settings disable all three, and the Go parent rejects a worker reporting an unsafe policy. |
| Revoked Nodes cannot obtain new swarm authorization | Pass with bounded residual risk | Core rechecks current `APPROVED/TRUSTED` state before every grant. Tracker peer records now expire no later than their grant. Existing peer connections are not an immediate malicious-node kill switch (`ARKSEC-006`). |
| Core APIs use least privilege | Partial | Node paths are identity- and resource-scoped, and tracker has no Core database. Core administration still uses one development bearer, while one Node key spans several federation capabilities (`ARKSEC-005`). |
| Secrets are not exposed in logs or UI | Partial | Application logs and browser transfer responses avoid grants and credentials. Removed local `.env`, private-key, database, log, and media artifacts; expanded ignore rules; added `scripts/check-security-hygiene.py`. Bearer URLs still require ingress redaction (`ARKSEC-007`), and global passwords still cross the Node boundary (`ARKSEC-001`). |
| Community Nodes do not receive Core administrative credentials | Pass | Core admin configuration exists only in the Core deployment. Node configuration contains only Node-local and Node federation settings. |

## Fixes made during review

- Removed bulk global-membership enumeration from Core, Agent, and Node UI.
- Bound tracker peer-record lifetime to the authorizing grant expiry, with a
  regression test where peer TTL is longer than grant lifetime.
- Removed secret-bearing/runtime artifacts found in this checkout, including
  non-example environment files and generated integration-test state.
- Expanded `.gitignore` for non-example `*.env`, generated `.arknet-*` state,
  keys, and runtime databases.
- Added a source-tree security-hygiene check and tests; it reports paths and
  categories without printing secret values.

## Open risk

The authoritative tracked list is [Security follow-ups](security-follow-ups.md).
`ARKSEC-001` and `ARKSEC-004` remain open release blockers for the stated
zero-trust and privacy architecture; `ARKSEC-003` was closed in Phase 19A and
`ARKSEC-002` in Phase 20B. The development-only Core admin token must not be
treated as production authorization. Node management now requires an operator
session; network reachability alone grants no management permission.

## Verification

The review gate is:

```sh
python3 scripts/check-security-hygiene.py
python3 -m pytest -q scripts/check_security_hygiene_test.py
(cd core && go test ./... && go vet ./...)
(cd node-agent && go test ./... && go vet ./...)
(cd tracker && go test ./... && go vet ./...)
(cd transfer && go test ./... && go vet ./...)
(cd node-ui && npm test -- --run && npm run build)
```

The host cannot run `transfer/worker_test.py` directly without Python
`libtorrent`. Run `worker_test` and `worker_seed_test` inside the built transfer
image, which includes real-libtorrent web-seed refresh and restart tests; the Go
parent fail-closed checks also cover the policy.
