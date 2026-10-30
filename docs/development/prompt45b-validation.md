# Prompt 45B validation — Node management permissions

Run date: 2026-10-28 AKDT. Base commit:
`255a9bc3e72ead42627a3b1edf30c1230a4319b6` on
`phase20/45b-enforce-matrix`, with the uncommitted Prompt 45B changes. This is
not a claim that the base commit alone implements operator enforcement.

## Executed evidence

Local run artifacts are under
`~/.hermes/cache/scratch/45b/` (not committed; no credentials in this report).

| Check | Result | Evidence |
| --- | --- | --- |
| Registered-route permission coverage and documentation parity | PASS | `node-agent/authz_test.go`: all registered routes must have a permission, and the documented matrix must match. Removing an entry or marking peer-serving PUT public was caught by mutation checks. |
| Deny non-operators before side effects | PASS | The generated spy test covers all 24 operator routes with anonymous, local-user bearer, global-member bearer, forged/lookalike cookie and mixed credentials. Repositories/services fail the test if reached. A valid operator session is a positive control. |
| Recent authentication for invitations | PASS | `TestInvitationRequiresRecentSignIn` rejects stale sign-in before the invitation handler runs. |
| Operator UI and HTTPS proxy unit tests | PASS | Component-gate UI step passed; login, setup, recovery-code display, logout, session expiry and no browser-storage writes are tested. HTTPS proxy tests exercise TLS minimum version, forwarding and cookie/security-header preservation. |
| MVP E2E with per-Node operator sessions | PASS, `EXIT 0` | `e2e2.log`, `e2e2.json`, run `arknet-p43-test-f4c29d9567ec`: all 17 items owned by this harness passed (2–16, 19, 20). Items 1, 17 and 18 are separate installer/swarm checks, not claimed by this result. |
| Node B management denial (item 20) | PASS | Node B returned 401 to anonymous callers, Node A's operator session and a global-member bearer on status, local users, invitations, peer-serving settings and update checks. Peer-serving state remained unchanged. |
| Whole-stack upgrade with operator bootstrap | PASS, `EXIT 0` | `upgrade4.log`: upgrade A→B printed a code that created an operator; printed fingerprint matched the certificate served by HTTPS. Existing data/volumes survived upgrade, failed-upgrade automatic rollback and manual rollback. Upgrade log contained no setup code. |
| Real Chromium over LAN HTTPS | PASS | `browser6/acceptance.log`, project `arknet-https-9599e66f87`: final reviewed UI image keeps pre-drop code root-owned, clears supplementary groups, and drops UID/GID after reading the key; password/TOTP sign-in, authenticated status, reload, logout, revoked-cookie replay and post-logout reload passed against real Agent/Postgres/UI. |
| Cookie flags and browser storage | PASS | `browser6/browser-state.json`: Secure, HttpOnly, SameSite=Strict, Path=/; cookie invisible to JavaScript; localStorage and sessionStorage empty before/after logout. |
| Plain HTTP exposure | PASS | Same browser harness: loopback HTTP returned 200, a TCP connection to the host LAN address on that HTTP port was refused, and LAN HTTPS served the UI. |
| Deployment-tool tests | PASS | `final-gate-review3/deploy-tests.log`: all 46 deployment tests passed, including certificate persistence, legacy UID-1000 ownership repair, explicit renewal/restart and bootstrap announcement. |
| Installer-check regression tests | PASS | `scripts.check_node_install_vm_test`: local-CLI status, setup-code redaction, fingerprint comparison and HTTPS/HTTP publication guards passed. |
| Fresh Ubuntu installer | PASS, `EXIT 0` | `install-vm.log`: fresh Ubuntu 24.04 without Docker reaches ENROLLMENT_PENDING, installer code is redacted, printed fingerprint matches the Node certificate, HTTPS is verified with that certificate, HTTP publication is loopback-only, and reinstallation preserves state/settings. |
| Product-path swarm | PASS, `EXIT 0` | `swarm.log`, `swarm.json`, run `arknet-p43f-test-ff80c619a93c`: operator-authenticated selection/start/settings paths retain the real libtorrent OFF/ON/OFF and fallback proof; scoped cleanup passed. |
| Agent tests with real PostgreSQL | PASS, `EXIT 0` | `agent-postgres.log`: full `go test -count=1 ./... && go vet ./...` runs in a Go container sharing the disposable PostgreSQL clock. Startup/restart/outage tests bootstrap an operator and explicitly attach the session cookie; restart preserves that session. |
| Final component gate | PASS, `EXIT 0` | `final-gate-review3.log`: final reviewed run; 12 steps passed, zero failed, including source hygiene, Go test/vet, UI tests/build, harness/deploy tests, real PostgreSQL and worker tests. |

## Completion and scope

**ARKSEC-002 is closed** by these executed checks. All disposable acceptance
stacks were cleaned up before the final component gate. The initial concurrent
gate correctly rejected active test-runtime directories; its startup/outage
failures were fixed by authenticating the tests, not weakening production
permissions.

The changes remain uncommitted on the branch named above. The separately
managed live-test VMs were not redeployed; existing deployments need the new
release and refreshed host tool before these protections apply.

No release-readiness claim follows from closing this one management-plane
boundary. ARKSEC-001, ARKSEC-004 and the other separately tracked issues remain.
