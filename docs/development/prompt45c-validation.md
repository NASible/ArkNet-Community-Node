# Prompt 45C validation — separate member listener

Run date: 2026-10-29 AKDT. The base commit was
`7bd1dd2` on `phase20/45c-separate-member-listener`, with the Prompt 45C
changes not yet committed. Those changes were then committed unchanged as the
Prompt 45C commit. After these runs, only the stale-harness follow-up commit
touched `scripts/check-node-stack.py` and `scripts/check-heartbeat-stack.py`.

## Executed evidence

Local run artifacts are under `~/.hermes/cache/scratch/45c/`. They are not
committed, and this report contains no credentials.

| Check | Result | Evidence |
| --- | --- | --- |
| Route partition, generated from registered routes | PASS | `TestListenerPartitionsFollowRegisteredPermissionMatrix` (`node-agent/authz_test.go`). Every operator and step-up route returns 404 on the member listener, and every public and member route returns 404 on the management listener, before anything behind the partition runs. On the correct listener, operator routes reach authorization (401 without a session) and public/member routes reach their handler. Mutation checks: serving nothing on either listener gave 34 failures; serving every route on the member listener failed the test. |
| `arknet-agent status` health probe | PASS | `TestDefaultAgentHealthProbesMemberListener` probes the member listener. It failed against the old code, which probed the management port. That defect made every upgrade, and its rollback, report the Agent unhealthy. |
| `.env` appends | PASS | `test_upgrade_appends_settings_safely_to_env_without_trailing_newline` failed first: the appended comment was joined onto the last setting. After the fix it passes, and `.env` keeps mode 0600. |
| Final component gate with E2E | PASS, `EXIT 0` | `gate5.log`: 15 steps passed, 0 failed. |
| MVP E2E | PASS | Gate step `mvp-e2e`, run `arknet-p43-test-8ec7b1e1af58`. |
| Whole-stack upgrade | PASS | Gate step `node-upgrade`: `ALL PASS` (19 checks), including listener separation (member status 404, management member-login 404), data preserved through upgrade and rollback, and no secrets in the upgrade log. |
| Product-path swarm | PASS | Gate step `product-swarm`, run `arknet-p43f-test-8010e3a7e1f2`: product-path evidence validated and scoped cleanup passed. |
| Real Chromium over LAN HTTPS | PASS, `EXIT 0` | `https3.log`, project `arknet-https-9560c8aee0`. The listeners refuse each other's routes, loopback HTTP is served, LAN HTTP is refused and LAN HTTPS works. Password/TOTP sign-in, reload, logout and revoked-cookie replay pass. The cookie is Secure, HttpOnly and SameSite=Strict, and browser storage is empty. |
| Deployment-tool tests | PASS | 47 tests, including the default member port `8086` and append-once behaviour. |
| Independent review | Done | No blockers. Fixed: one-directional partition test; member port `8083` colliding with Core's default (now `8086`); operator routes exposed on live-test `member-x` hostnames (now 404); `node-ui` README; `.env` newline join. Deliberately unchanged: the upgrade harness keeps seeding a test-only member port. |

## Harness fixes found during validation

- **Swarm harness (`check-multi-node-swarm-stack.py`).** Node A's transfer
  sidecar joined Node B's network through Compose, which registered the
  `arknet-transfer` service name there too. Node B's Agent could then resolve
  its own sidecar to Node A's, get a 401 for the wrong token, and report
  `transfer_unavailable`. The harness now attaches the network with
  `docker network connect` and then restarts the sidecar, because libtorrent
  opens listen sockets only on interfaces present at startup.
- **Integration tests.** The tests now wait for both `server_started`
  addresses. Each line is tagged with `listener`. Sign-in uses the member
  address and status uses the management address.

## Scope

The live-test VMs were not redeployed. Their new ingress (member port `8086`,
operator routes blocked on `member-x`) has been checked by syntax only.
