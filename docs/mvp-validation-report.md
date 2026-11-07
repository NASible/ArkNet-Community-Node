# ArkNet validation report

Current report: **Phase 20 (Prompt 50), 2026-11-06 America/Anchorage (AKST)**,
on frozen commit **`b232f460874141d8e5a02ef3ebfdde1875153cf5`** (`b232f46`).
Result: every acceptance check passed on that commit. The Phase 19A report
(2026-10-27) is kept below as history.

## Verdict

- **Is ArkNet functionally complete on the frozen tree? Yes.** The one-command
  gate passed 18 of 18 steps (`SUMMARY: 18 passed, 0 failed`, `EXIT 0`). It
  covers component tests, real-PostgreSQL store tests, the TLS ingress
  sentinel check, MVP E2E items 2–16, 19 and 20 (relay-only peer transport),
  whole-stack upgrade, the product swarm and a fresh-VM install. The
  standalone ARKSEC checks for operator HTTPS, scoped Core administration and
  enrollment abuse controls also passed. The live-test group was updated to
  the same commit and all three of its verifiers passed.
- **Is it ready for hostile multi-operator production? No, not yet.**
  ARKSEC-004 is closed, so the remaining risk is not the private-relay gap.
  Every code-level acceptance criterion for ARKSEC-001 through ARKSEC-008
  passes on this commit. What remains:
  1. **ARKSEC-009 is open.** It needs an operator to confirm that credentials
     which may have escaped through removed local `.env` files and generated
     artifacts were rotated or were disposable. That is an operator action, not
     a test. It is done only for the live-test group's application secrets;
     the live tracker signing key and other environments are not covered.
  2. **Residual risks.** These are documented, accepted for the first release,
     and still apply to hostile operators:
     - **Grant window.** A malicious Node keeps a bounded authorization window
       for grants it already holds, up to the 5-minute grant lifetime (see
       [Node revocation](node-revocation.md#malicious-node-residual-window-arksec-004)).
     - **Rate limits.** Login and enrollment limits are per Core process and
       keyed by socket peer, so they reset on restart and are shared by
       clients behind a proxy.
     - **Relay.** The first release has a single operator relay. Community
       relays are not shipped.
     - **No external audit.** No independent security audit or penetration
       test has been done.
  3. **Deployment.** The only deployment is a private, trusted-operator
     four-VM pilot.

## Evidence rules

- **PASS** means the named check logged PASS in a run on commit `b232f46`, and
  the run's own exit line (`EXIT 0`, or the gate summary) confirms it. Standalone
  and live logs start with their `COMMIT`; gate step logs are identified by
  `gate.out`'s first line, `ArkNet gate: b232f46`. Unit and component tests never promote an item
  that has an end-to-end check.
- The tree was clean before and after every run.
  `~/.hermes/cache/scratch/p50/run-frozen.sh` refuses a dirty tree and
  re-checks `HEAD` and `git status` after each check. Its record ends with
  `FROZEN_COMMIT_STILL b232f46…`.
- Logs are local evidence on the validation host and are not committed:
  - local runs: `~/.hermes/cache/scratch/p50/frozen-b232f46/`
  - live-test runs: `~/.hermes/cache/scratch/p50/live-b232f46/`
- After the freeze, only documentation and the live-test proof JSONs (written
  by the verifiers) changed.

## Runs on `b232f46`

| ID | Log | Command | Time (AKST) | Outcome |
| --- | --- | --- | --- | --- |
| G | `gate.out`, `gate/*.log` | `scripts/check-all.sh --e2e --vm --logs …/gate` | 19:28–20:15 | First line `ArkNet gate: b232f46`. `SUMMARY: 18 passed, 0 failed`, `EXIT 0`. |
| E | `gate/mvp-e2e.log`, `gate/e2e.json` | gate step `mvp-e2e` (`check-mvp-e2e.py`), run `arknet-p43-test-4a9d8a0e1b59` | in G | All 15 criteria PASS. `PASS: container logs contain 0 known secret values` (line 674). `PASS: exact scoped cleanup` (line 778). |
| U | `gate/node-upgrade.log` | gate step `node-upgrade` | in G | `ALL PASS` (line 89). |
| P | `gate/product-swarm.log`, `gate/swarm.json` | gate step `product-swarm`, run `arknet-p43f-test-bc5b0abdcd6b` | in G | `PASS: Prompt 43F product-path evidence validated` (line 460). Cleanup PASS (line 602). |
| I | `gate/node-install-vm.log` | gate step `node-install-vm` (fresh Lima Ubuntu VM) | in G | `ALL PASS` (line 73). |
| R | `gate/ingress-redaction.log` | gate step `ingress-redaction` | in G | `PASS: five real sinks, verified TLS, forwarding, forced errors, negative control`. Exact cleanup PASS. |
| C | `gate/{go-*,node-ui,harness-tests,deploy-tests,postgres-tests,worker-tests}.log` | gate component steps | in G | All PASS. Covers five Go modules, the UI (276 tests), harness tests (122, one opt-in skip that R executes), real-PostgreSQL Core and Agent store tests, and worker tests in the real libtorrent image. |
| H | `operator-https.log` | `check-operator-https.py` (real Chromium, LAN HTTPS) | 20:15–20:16 | `EXIT 0`. |
| A | `core-admin-stack.log` | `check-core-admin-stack.py` | 20:16–20:17 | `EXIT 0`. |
| N | `enrollment-stack.log` | `check-enrollment-stack.py` | 20:17–20:22 | `EXIT 0`. |
| L | `live-b232f46/*.log` | `run-live.sh` from a clean `b232f46` worktree: build, distribute, deploy (Core, A, B, C), `verify-deployment.py`, `prove-media.py` (freshly generated sample), backups, `pull-backups.py`, `verify-backups.py` | 19:28–19:32 | Every step `EXIT 0`. Results: `DEPLOYMENT_AND_ACCESS_BOUNDARY_PROOF_PASS`, `ALL_THREE_NODE_MEDIA_PROOF_PASS`, `BACKUP_DECRYPT_AND_RESTORE_PASS` for all four VMs. A scan found 0 admin tokens, grant-like strings or private keys in the logs. |

## Prompt 43 items on the frozen tree

E covers items 2–16, 19 and 20. Item 1 comes from I; items 17 and 18 come from U.
Line numbers refer to `gate/mvp-e2e.log` unless another log is named.

| # | Item | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Fresh node installation | **PASS** | I lines 61–73: the installer exited 0 and the fresh VM reached ENROLLMENT_PENDING. Core listed it as pending. Permissions were safe, a rerun was safe, and no secrets appeared in output. |
| 2 | Enrollment | **PASS** | Lines 566–568: signed outbound self-enrollment, then approval. Both Nodes reached CONNECTED with distinct pinned keys. |
| 3 | Node restart | **PASS** | Lines 597–598. |
| 4 | Global user | **PASS** | Lines 569–570. |
| 5 | Local-only user | **PASS** | Lines 571–572. |
| 6 | Pairwise node membership | **PASS** | Lines 573–574. |
| 7 | Jellyfin | **PASS** | Lines 575–576. |
| 8 | Media selection | **PASS** | Lines 577–578. |
| 9 | BitTorrent v2 Core → Node transfer | **PASS** | Lines 599, 609, 611: exact SHA-256 and a signed replica report. |
| 10 | Interrupted transfer/resume | **PASS** | Lines 604, 610, 612. |
| 11 | Node A → Node B swarm contribution | **PASS** | Lines 637–652. Node A's libtorrent contributed 33,581,357 bytes over the private relay, and B completed the exact SHA-256 with its web seed disconnected. Every outgoing attempt was gateway-classified (no direct path). With all relays down, 0 late peer bytes moved and the transfer did not fall back to direct. P also shows payload forwarded by the relay (4,202,675 bytes). |
| 12 | Offline Jellyfin playback | **PASS** | Lines 625–626. |
| 13 | Core-bound global authentication | **PASS** | Lines 613–628. The password was typed only at Core. Replayed and cross-Node assertions were rejected. In the malicious-Node harness, none of 11 captured values was usable at Core or Node B. Offline login stayed within its window and was refused after it. |
| 14 | Core reconnect | **PASS** | Lines 621–622, 635–636. |
| 15 | Removed-media retention | **PASS** | Lines 653–655. |
| 16 | Node revocation | **PASS** | Lines 664–673. Core push closed the live relay session, and relay-only bytes stopped 0.77 s after revocation. B converged to REVOKED/CANCELLED, refused new transfers (409 `node_revoked`) and kept its retained media exact. A stayed CONNECTED. |
| 17 | Update discovery | **PASS** | U line 5: `UPDATE_AVAILABLE`, current `0.0.2-dev.1`, available `0.0.2-dev.2`, while A kept running. |
| 18 | Whole-stack upgrade | **PASS** | U lines 43–89: upgrade, no-op rerun, unpullable abort, automatic rollback (exit 3), manual rollback, data preserved, no secrets. |
| 19 | Operator cannot enumerate global users | **PASS** | Lines 656–657. |
| 20 | Node A cannot arbitrarily access Node B | **PASS** | Lines 658–663. Node B's management address returned 401 to anonymous callers, Node A's operator session and member bearers. Listener separation held. |

### Product swarm and Peer Serving measurements (P)

Peer transport: `private relay; no Node-to-Node network path`. The swarm
check also asserts that the two transfer containers share no network.

| Phase | Node A transmitted | Notes |
| --- | --- | --- |
| Peer Serving OFF, Node B downloading | 1,228 bytes | Under the 128 KiB control bound. |
| Peer Serving ON | 4,119,854 bytes | The relay forwarded 4,202,675 bytes in the same window. Core egress fell from 26,732,194 bytes (Core-only download) to 22,495,232. |
| Peer Serving turned OFF during the session | 0 bytes | Node B completed the exact object, SHA-256 `3e7e42d4…`. |

## ARKSEC status on the frozen tree

| ID | Status | Evidence on `b232f46` |
| --- | --- | --- |
| [ARKSEC-001](security-follow-ups.md#arksec-001) | **Closed** | E item 13: one-time Node-bound assertions, replay and cross-Node rejection, and a malicious-Node capture harness. |
| [ARKSEC-002](security-follow-ups.md#arksec-002) | **Closed** | E item 20 (operator-session denial and listener separation), H (real Chromium LAN HTTPS login and logout), U. Component route/permission matrix and spy tests are in C. |
| [ARKSEC-003](security-follow-ups.md#arksec-003) | **Fixed** | P: OFF 1,228 bytes, ON 4,119,854 bytes, after OFF 0 bytes, now over the relay. |
| [ARKSEC-004](security-follow-ups.md#arksec-004) | **Closed** | E item 11 (relay-only payload, no direct attempts, no direct fallback with relays down) and item 16. P measures the relay path with no Node-to-Node network. The malicious-Node residual window is documented. |
| [ARKSEC-005](security-follow-ups.md#arksec-005) | **Closed** | A (scoped tokens, denial before repositories, audit, revocation, the removed shared bearer refuses to start). C (Core spy and real-PostgreSQL audit tests). E and L mint scoped, short-lived tokens. |
| [ARKSEC-006](security-follow-ups.md#arksec-006) | **Closed for cooperative Nodes** | E item 16: active transfer cancelled, bytes stop, terminal REVOKED. Malicious-Node enforcement is covered by the relay (ARKSEC-004). |
| [ARKSEC-007](security-follow-ups.md#arksec-007) | **Closed for the supported recipe** | R: verified TLS, real Core/tracker/Agent/UI, raw-log sentinel scan of five sinks, forced errors, and a leak-detecting negative control. L: live ingress request-error sink set to `/dev/null`, and no request logging. |
| [ARKSEC-008](security-follow-ups.md#arksec-008) | **Closed for the scoped criteria** | N (signed enrollment, generic receipts, restart idempotency). C (rate limit, quota and expiry tests, real PostgreSQL). E passes under the production limits. |
| [ARKSEC-009](security-follow-ups.md#arksec-009) | **Open** | Needs operator confirmation, which no test can provide. Live-test application secrets were rotated in Prompt 44B; the live tracker signing key and other environments are not covered. Prompt 49 removed the shared development admin bearer from the product (A proves a configured one stops Core). |

## Live-test group (L)

- Core, tracker, the operator relay and PostgreSQL run on the Core VM; each Node
  runs Agent, transfer, UI, PostgreSQL and Jellyfin. Every container is healthy
  and its image ID matches `deploy/live-test/build-record.json`
  (source SHA-256 `e5961872…`, built from the clean `b232f46` worktree).
- **Relay exposure.** The relay is reachable only from the three Node VPN
  addresses, over TLS 1.3, and serves exactly the certificate its SPKI pin is
  derived from. It is blocked on Core's LAN address and from the operator Mac.
- **Management access.** Node management refuses anonymous callers (401).
  Verifiers use per-Node operator sessions and scoped Core admin tokens.
- **Media proof.** A freshly generated sample was downloaded by all three Nodes
  through the real private v2 workers, finalized with matching SHA-256 and size,
  indexed by Jellyfin through the Agent and direct-streamed with matching bytes.
- **Backups.** Fresh backups were taken after the rollout, pulled off-VM,
  decrypted and restored into disposable databases on all four VMs.
- **Reboots (run after the report, with operator approval, 20:33–20:36 AKST):**
  `verify-reboots.py` rebooted each VM from its own OS, one at a time. Every VM
  came back with a new boot ID, active nginx and all containers healthy (Core 4,
  each Node 5), and the Core relay socket proxy was listening again
  (`ALL_FOUR_VM_REBOOTS_RECOVERED`, `EXIT 0`). A following `verify-deployment.py`
  passed again, including relay TLS 1.3 and the certificate pin from every Node
  (`DEPLOYMENT_AND_ACCESS_BOUNDARY_PROOF_PASS`, `EXIT 0`). Logs:
  `~/.hermes/cache/scratch/p50/live-reboots/`. The deployed services were the
  `b232f46` build. The only tooling changes since the freeze are this reboot
  verifier (no Proxmox credential needed; expects the relay) and bounded
  rollback-copy retention in `deploy-stack.py`; neither changes deployed images
  or configuration.

Proof JSONs: [deployment](../deploy/live-test/deployment-proof.json),
[media](../deploy/live-test/sample-proof.json),
[backups](../deploy/live-test/backup-proof.json),
[build](../deploy/live-test/build-record.json),
[reboots](../deploy/live-test/reboot-proof.json).

## Defects fixed during Prompt 50

Every defect below is a harness or tooling defect, not a product defect. Each
was fixed with a regression test before the final freeze.

- **Product swarm still assumed direct peering.** Since 48C the tracker
  requires relay keys, and peers have no shared network. The harness now uses
  the shipped relay overlay, proves the transfer containers share no network,
  and requires relay-forwarded payload (`c34b664`).
- **Stale fixture size on the shared folder.** The Colima shared folder showed
  the torrent helper a stale 1,753,434-byte view of a padded 96 MiB fixture,
  which failed E item 16 at `74d7a2e`. The worker correctly rejected the
  mismatched metadata. The helper now waits for the host size (`7e5239d`) and
  reports payload bytes without v2 pad files (`b232f46`).
- **Live tooling predated the relay and operator sessions.** It now covers the
  relay, operator sessions, Node leaf-certificate names and Jellyfin's
  grouping of sample versions (`74d7a2e`, `7e5239d`).

Earlier frozen attempts (`74d7a2e`: 17/18; `7e5239d`: 17/18) are superseded.
No PASS above comes from them.

## Completion boundary

Any runtime change after `b232f46` requires rerunning G, H, A, N and L.
Hostile multi-operator readiness additionally requires:

- ARKSEC-009 to be closed by operator confirmation;
- an explicit decision on the documented residual windows;
- multi-relay and production deployment evidence beyond the private pilot.

---

# Historical: Phase 19A final-tree report (2026-10-27)

This section is preserved unchanged except for heading levels. Its ARKSEC
statuses are a dated snapshot; the table above and
[Security follow-ups](security-follow-ups.md) are current.

Run date: **2026-10-27 (America/Anchorage, AKDT)**. All 20 Prompt 43 items were
run on one frozen source tree. Result: **20 PASS, 0 FAIL**.

**Subsequent security change:** Prompt 45B closed ARKSEC-002 on 2026-10-28
with operator permission enforcement, bootstrap/upgrade and browser HTTPS proof.
See [Prompt 45B validation](development/prompt45b-validation.md). The dated Phase
19A findings below remain a historical snapshot; use
[Security follow-ups](security-follow-ups.md) for current statuses.

### Verdict

- **Is the MVP functionally complete? Yes.** Every Prompt 43 item passes in an
  end-to-end run on the final tree. Fresh install, update discovery and
  whole-stack upgrade pass in their own final-tree checks. The standalone
  product swarm check also passes, with Peer Serving OFF/ON/OFF measurements.
- **Was it ready for hostile multi-operator production at this run? No.** The
  functional table did not change this answer. The reusable global password
  crossed the untrusted Node (ARKSEC-001), and the Node management API lacked
  operator authentication (ARKSEC-002, subsequently closed by Prompt 45B).
  Private relay was not the default (ARKSEC-004). Revocation did not terminate
  active transfers (ARKSEC-006). The remaining medium items were also open.

### Evidence rules

- **PASS** means a step logged `PASS` in a run on the final tree (defined
  below), and that run's log ends with `EXIT 0`. Unit and component tests
  never promote an item where an E2E check exists.
- Each run's verdict comes from the `EXIT` line in its log, not from the
  wrapper's exit code.
- The logs are local evidence artifacts on the validation host, under
  `~/.hermes/cache/scratch/mvp-e2e/final2-2026-10-27/`. They are not
  committed.

#### Final tree

The checkout has no `.git` metadata, so the tree is identified by a hash
manifest. `tree-hash.sh` hashes every file under `core/`, `node-agent/`,
`node-ui/` (excluding `node_modules`/`dist`), `transfer/`, `tracker/`,
`schemas/`, `scripts/`, `deploy/node/`, `deploy/core/` and `VERSION`. The
manifest was taken before the E2E run and again after the last check. Both
manifests are identical:

```text
tree manifest sha256  7b71aee921a4992002d82e843b49ab85a381a64526cb3c9acbbe707f7db9a452
```

Run P started at 17:56:37, before the manifest was taken. No hashed file was
modified after that time; only regenerated `node-ui/dist` build output, which
is excluded, changed. All earlier runs (2026-10-25 and the first 2026-10-27
pass) predate the fixes below and are not used here.

#### Runs

| ID | Log (in `final2-2026-10-27/`) | Command | Time (AKDT) | Outcome |
| --- | --- | --- | --- | --- |
| E | `e2e.log`, `e2e.json` | `python3 scripts/check-mvp-e2e.py --results …/e2e.json` | 18:11–18:15 | 17/17 criteria PASS. `PASS: exact scoped cleanup` (line 647). `EXIT 0` (line 667). |
| U | `upgrade.log` | `python3 scripts/check-node-upgrade-stack.py` | 18:16–18:17 | `ALL PASS`, `EXIT 0` (line 77). |
| I | `install-vm.log` | `python3 scripts/check-node-install-vm.py` | 18:17–18:19 | Fresh Lima Ubuntu 24.04.4 LTS VM. `ALL PASS`, `EXIT 0` (line 57). VM removed afterwards. |
| P | `swarm.log`, `swarm.json` | `python3 scripts/check-multi-node-swarm-stack.py --results …/swarm.json` | 17:56–18:10 | Product path, no adapter. `PASS: Prompt 43F product-path evidence validated` (line 414). Cleanup PASS (line 550). `EXIT 0` (line 551). |
| W | `worker-tests.log` | `worker_test` and `worker_seed_test` inside the transfer image (real `python3-libtorrent`) | 18:16 | 29 tests, `OK`, `EXIT 0`. Component evidence only. |
| T | `harness-tests.log` | `python3 -m unittest scripts.mvp_e2e_test scripts.multi_node_swarm_evidence_test scripts.check_multi_node_swarm_stack_test scripts.check_security_hygiene_test` | 18:11 | 23 tests, `OK`, `EXIT 0`. Component evidence only. |
| G | `go-tests.log`, `hygiene.log` | `go test ./... && go vet ./...` for `core`, `node-agent`, `tracker` and `transfer`; `scripts/check-security-hygiene.py` | 18:11–18:16 | All four modules `EXIT 0`. Hygiene check passed, `EXIT 0`. Component evidence only. |

### Prompt 43: all 20 items

Every row is from the final tree on 2026-10-27. `check-mvp-e2e.py` covers
items 2–16, 19 and 20. Item 1 comes from I, items 17 and 18 from U. Line
numbers refer to the named log.

| # | Item | Result | Check script and step | Run date | Evidence |
| --- | --- | --- | --- | --- | --- |
| 1 | Fresh node installation | **PASS** | `check-node-install-vm.py`: fresh VM → documented installer → `ENROLLMENT_PENDING` | 2026-10-27 | I lines 2–5, 44–57: fresh Ubuntu 24.04.4 LTS with no Docker. The installer exited 0 and the Node reached ENROLLMENT_PENDING, listed pending in Core. Preflight failures leave the VM unchanged, permissions are safe, a rerun is safe, and no secrets appear in output. |
| 2 | Enrollment | **PASS** | `check-mvp-e2e.py`: `enrollment()` | 2026-10-27 | E lines 466–468: both Nodes self-enrolled outbound and were approved. Both reached CONNECTED with two distinct pinned keys. |
| 3 | Node restart | **PASS** | `check-mvp-e2e.py`: `restart()` | 2026-10-27 | E lines 497–498: an Agent restart and a container recreation kept the Node ID, CONNECTED enrollment, queued selection and local users. Core still had exactly 2 Nodes. |
| 4 | Global user | **PASS** | `check-mvp-e2e.py`: `global_user()` | 2026-10-27 | E lines 469–470: Core created the user, issued a session and rejected a wrong password. |
| 5 | Local-only user | **PASS** | `check-mvp-e2e.py`: `local_user()` | 2026-10-27 | E lines 471–472: the local user authenticates on Node A only. The Core database dump has no trace of it. |
| 6 | Pairwise node membership | **PASS** | `check-mvp-e2e.py`: `pairwise()` | 2026-10-27 | E lines 473–474: one global user got different random member IDs on A and B. Neither Node stores the username, the global ID, or the other Node's identifiers. |
| 7 | Jellyfin | **PASS** | `check-mvp-e2e.py`: `jellyfin()` | 2026-10-27 | E lines 475–476: both Agents report local Jellyfin healthy, with the ArkNet library on read-only media. |
| 8 | Media selection | **PASS** | `check-mvp-e2e.py`: `selection()` | 2026-10-27 | E lines 477–478: the signed catalog fetch listed the object, and operator selection queued it. |
| 9 | BitTorrent v2 Core → Node transfer | **PASS** | `check-mvp-e2e.py`: `transfer()` | 2026-10-27 | E lines 499, 509, 511: a real libtorrent transfer from the Core web seed finished AVAILABLE with an exact SHA-256 match and a signed replica report in Core. |
| 10 | Interrupted transfer/resume | **PASS** | `check-mvp-e2e.py`: `transfer()` restart/resume phase | 2026-10-27 | E lines 504, 510, 512: Core stopped and the transfer container restarted mid-download, keeping 10,371,072 bytes. A fresh authorization then resumed the retained pieces to completion. |
| 11 | Node A → Node B swarm contribution | **PASS** | `check-mvp-e2e.py`: `swarm()`; `check-multi-node-swarm-stack.py`: product-seed proof | 2026-10-27 | E lines 533–535: path was Agent seed reconciler → transfer service → libtorrent worker. With Core's web seed unreachable, Node B completed the exact object from Node A over the private, tracker-gated v2 swarm. P line 413: with Peer Serving ON, Node A uploaded 3,725,555 bytes. Node B completed the exact 26,270,656-byte object. |
| 12 | Offline Jellyfin playback | **PASS** | `check-mvp-e2e.py`: `offline_playback()` | 2026-10-27 | E lines 521–522: with Core and the tracker stopped, Jellyfin on Node A streamed the exact completed MP4. |
| 13 | Cached global authentication | **PASS** | `check-mvp-e2e.py`: `online_global_login()`, `offline_auth()` | 2026-10-27 | E lines 513–514, 523–524: an online login was verified by Core and cached on the Node. With Core offline, the cached member authenticates, a wrong password and B's member ID are rejected, and the local user still works. This is functional only and does **not** resolve ARKSEC-001. |
| 14 | Core reconnect | **PASS** | `check-mvp-e2e.py`: `offline()`, `reconnect()` | 2026-10-27 | E lines 519–520, 531–532: during the Core outage, Node A went DEGRADED and kept serving. After Core was restored, A returned to CONNECTED automatically with no re-enrollment. |
| 15 | Removed-media retention | **PASS** | `check-mvp-e2e.py`: `removed()` | 2026-10-27 | E lines 536–538: the Core tombstone propagated and Node A went REMOVED_RETAINED. The retained file stayed byte-identical and playable in Jellyfin. Reselection was refused, and A is no longer an eligible source. |
| 16 | Node revocation | **PASS** | `check-mvp-e2e.py`: `revocation()` | 2026-10-27 | E lines 545–548: the revoked Node B was refused a new swarm authorization and entered terminal REVOKED. Node A stayed CONNECTED. Active-transfer termination is not tested (ARKSEC-006). |
| 17 | Update discovery | **PASS** | `check-node-upgrade-stack.py`: Agent discovers release B while A keeps running | 2026-10-27 | U line 4: `UPDATE_AVAILABLE`, current `0.0.2-dev.1`, available `0.0.2-dev.2`. |
| 18 | Whole-stack upgrade | **PASS** | `check-node-upgrade-stack.py`: A → B upgrade, preservation, automatic and manual rollback | 2026-10-27 | U lines 22–25, 41, 61–62, 73–77: A → B upgraded via `arknet-node` (exit 0) with identity, settings, Jellyfin data and volumes preserved. An unpullable release aborts with no change. A failing release is rolled back automatically (exit 3). Manual rollback returns to A. Data is preserved throughout. `EXIT 0`. |
| 19 | Operator cannot enumerate global users | **PASS** | `check-mvp-e2e.py`: `enumeration()` | 2026-10-27 | E lines 539–540: the listing routes return 404 and Core has no user listing. Operator APIs and the UI bundle carry no global username or ID. Line 549: container logs contain 0 known secret values. |
| 20 | Node A cannot arbitrarily access Node B | **PASS** | `check-mvp-e2e.py`: `isolation()` | 2026-10-27 | E lines 541–544: from A's containers, B's Agent, UI, PostgreSQL and Jellyfin are unreachable. B's peer control API returns 401. A's credential is rejected (401) on B's Core route. This does **not** resolve ARKSEC-002. |

#### Peer Serving measurement (P, line 413)

These are byte counts on Node A's transfer container interfaces, from run P.

| Phase | Node A transmitted | Notes |
| --- | --- | --- |
| Peer Serving OFF, Node B downloading | 2,900 bytes | Under the 128 KiB control-traffic bound. Smaller than one 16 KiB BitTorrent block, so no payload block can have been sent. Core advertised no eligible source. |
| Peer Serving ON | 3,725,555 bytes | Served only after Core source eligibility and a fresh seed grant. The worker hash-checked the file and reached `SEEDING`. |
| Peer Serving turned OFF during an active session | 0 bytes | The seed was removed within one interval. Node B continued from Core and completed the exact object. |

### Phase 19A blockers fixed

"Implemented" and "proven live" are different claims, and each entry says
which one applies. "Proven" here means proven in the final-tree runs above.

- **43A: Core never started in the E2E harness (harness bug).** On macOS,
  Docker Desktop could not see the temporary bind-mount sources. The fix uses
  repository-local work directories and probes bind sources before starting.
  It also added `--setup-only`. **Proven:** E.
- **43B: upgrade check failed at "release A installed and running" (harness
  bug).** Fixed with repository-local release fixtures and a mount preflight.
  Real Agent update discovery was added to the check. **Proven:** U.
- **43C: no product seeding path in the worker.** Added runtime-only
  `seed`/`unseed` with a strict `media_<hex>.mp4` mapping, a full hash
  recheck, upload-only mode, no web seeds, and DHT/PEX/LSD off. **Proven:** P
  and E item 11.
- **43D: no seed API in the transfer service.** Added bearer-protected
  `POST`/`GET`/`DELETE /api/transfer/v1/seeds` with backend and Agent
  clients. **Proven:** P and E item 11.
- **43E: no Agent Peer Serving lifecycle.** Added seed-only grants and a seed
  reconciler that unseeds on OFF, removal, Core 401/403, or a Node that is not
  CONNECTED. **Proven:** P (OFF/ON/OFF).
- **43F: the swarm proof used a test-only seeder.** The adapter was removed.
  **Proven:** P is product-path, with `product_path: true` in `swarm.json`.
- **43G cycle 1: E2E enrollment SQL (harness bug).** The query used a
  nonexistent `node_credentials` table; it now uses
  `nodes.enrollment_public_key`. **Proven:** item 2.
- **43G cycle 2: libtorrent bypassed rate limits on private networks (product
  bug).** libtorrent defaults to `ignore_limits_on_local_network=true`; the
  worker now disables it explicitly. **Component evidence:** W.
- **43G cycle 3: test reconcile interval raised from 5 to 30 seconds (harness
  setting).** This only reduced source churn; it did not fix it. The real
  cause is the first remediation item below.
- **43H: install and upgrade evidence did not match the final tree.** Both
  checks were rerun. **Proven:** I and U.
- **Remediation: web-seed source churn (product bug in `transfer/worker.py`).**
  Every Agent authorization refresh removed and re-added the web-seed URL.
  That aborted libtorrent's in-flight range request, which covers the rest of
  the file, and discarded its data. In an isolated reproduction, a 5-second
  refresh moved a 16 MiB transfer 0 bytes in 110 s. The same transfer
  finishes in 34 s without refresh. Fix: trackers still refresh every time;
  the URL seed is kept while it is exactly the worker-authorized URL on the
  same handle and younger than 120 s (Core grants last 5 min); any other URL
  seed is removed immediately. Regression tests:
  `test_refresh_keeps_live_web_seed_until_rotation_age` and
  `test_frequent_authorization_refresh_does_not_starve_web_seed_transfer`.
  **Proven:** items 9 and 11, and P.
- **Remediation: resumed transfers had no metadata (product bug in
  `transfer/worker.py`).** After a worker restart, the handle was rebuilt from
  legacy resume data with no info dict. It stayed in `downloading_metadata`
  forever, because a web seed cannot supply metadata. Fix: attach the
  validated Core-fetched metadata to every resumed handle, and validate it
  before reading resume data so bad metadata never discards progress.
  Regression test:
  `test_restart_mid_download_restores_metadata_and_resumes_to_completion`.
  **Proven:** item 10. Items 12 and 15 depended on 9 and 10 and now pass.
- **Remediation: the swarm check waited for a transfer nothing starts (harness
  bug).** `check-multi-node-swarm-stack.py` selected media on Node B and
  waited for the reconciler to start it. In the product, only the operator's
  explicit `transfer/start` moves a selection from QUEUED to a transfer, as
  documented. The check now calls `transfer/start`, as Node A and the E2E
  already did. **Proven:** P.

### Known limitations

The acceptance criteria for each ID are tracked in
[Security follow-ups](security-follow-ups.md).

| ID | Limitation | Related items |
| --- | --- | --- |
| [ARKSEC-001](security-follow-ups.md#arksec-001) | **Open, critical.** The global password transits the Node, and the Node keeps a password-derived offline verifier. A malicious operator can capture a credential that works at Core. | 13 passes functionally only. |
| [ARKSEC-002](security-follow-ups.md#arksec-002) | **Closed later (Prompt 45B).** At this Phase 19A run the management API was unauthenticated; Prompt 45B added Node-local operator sessions, route-matrix/spy enforcement and LAN HTTPS. | Prompt 45B reran operator-authenticated item 20 and the full E2E; see its validation report. |
| [ARKSEC-003](security-follow-ups.md#arksec-003) | **Fixed.** Live product measurement P meets the acceptance criteria. The OFF window has a 2,900-byte total-transmit bound rather than a literal payload counter. | 11 |
| [ARKSEC-004](security-follow-ups.md#arksec-004) | **Open, high.** Private relay is not the default. Item 11 uses peers connected directly on a private Docker network, which does not prove origin-address privacy. | 11 |
| [ARKSEC-005](security-follow-ups.md#arksec-005) | **Closed later (Prompt 49).** Scoped administrator tokens replace the shared bearer; per-Node capabilities narrow the enrollment key. Denied calls never reach repositories and administrator actions are attributable in an append-only audit log. | Fresh MVP E2E `arknet-p43-test-68e413875194`: 17/17; [validation](development/prompt49-validation.md). |
| [ARKSEC-006](security-follow-ups.md#arksec-006) | **Open.** Active transfers on a revoked Node are not forcibly terminated. A revoked Node cannot get new grants, and the cooperative reconciler unseeds when not CONNECTED. Active downloads are not cancelled, and established peer sessions are not cut. | 16 does not test byte-counter termination. |
| [ARKSEC-007](security-follow-ups.md#arksec-007) | **Open.** There is no supported TLS ingress that redacts bearer URLs. The tests use explicit HTTP allowances. | — |
| [ARKSEC-008](security-follow-ups.md#arksec-008) | **Open.** Login and enrollment have no abuse controls, and the unsigned enrollment compatibility path still exists. | — |
| [ARKSEC-009](security-follow-ups.md#arksec-009) | **Open.** Removed local artifacts still need operator-confirmed credential rotation. Addressed for the live-test group only by Prompt 44B (after this report's run). | — |

Other limitations, not security items:

- A kept web-seed URL is reused for new range requests until it is rotated.
  Rotation happens on the first refresh after 120 s. With
  `ARKNET_TRANSFER_RECONCILE_INTERVAL` above 3 minutes, the kept URL's grant
  can expire before then; libtorrent then gets a 403 and drops the seed, and
  the next refresh installs a fresh one. The tested interval is 30 s.
- The real libtorrent transfer service logs `"mock":true` in its
  `server_started` line (`transfer/server.go`). Its HTTP health and version
  responses correctly report `mock:false`, and the harnesses check those.
  The log field is wrong.

### Completion boundary

The MVP is functionally complete on the tree above. Any runtime change after
it requires rerunning E, I, U and P. Hostile multi-operator readiness requires
closing ARKSEC-001, 002, 004 and 006 at minimum. Passing functional checks
cannot waive those trust boundaries, and neither can unit tests.
