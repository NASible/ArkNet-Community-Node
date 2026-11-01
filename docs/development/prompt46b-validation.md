# Prompt 46B validation: Agent accepts assertions; password path removed

Date: 2026-10-31. Branch `phase20/46b-agent-assertions`.

## Acceptance criteria

| Criterion | Evidence |
| --- | --- |
| Malicious-Node E2E step | MVP E2E run `arknet-p43-test-0737adcda8fa`, item 13: a recording relay (`scripts/global_login.py` `RecordingRelay`) in front of Node A's member listener captured 941 bytes during a login. None of the 11 token-like values it extracted contained the password or the Core session. None signed in at Core as a password or as a bearer token. None opened a session at Node B as an assertion (with Node B's own pending login, and with Node A's recorded login cookie) or as a bearer token. Node A's device credential was refused by Node B. |
| Item 13 rewritten and passing | Same run, item 13 "Core-bound global authentication" PASS: online login through Core's page; offline login inside the 180 s test window with Core stopped; `401 offline_login_rejected` and an ended offline session after the window; replay rejected by Node A (same and new login); Node A's assertion rejected by Node B; the old password route answers `410`. |
| ARKSEC-001 closed, docs updated | `docs/security-follow-ups.md`, `docs/global-users.md`, `docs/cached-global-authorization.md` (rewritten), `docs/global-login-assertions.md` (Agent implementation section). |

The same run passed every other MVP item (2–16, 19, 20), and its container logs
contained 0 known secret values. Those values include the global password,
every assertion, session token and cookie value, and the assertion signing key.

## Tasks

1. **Agent accepts assertions.** `node-agent/internal/globalauth/assertion.go`
   is byte-compatible with Core. A golden assertion issued by Core's signer is
   verified by tests in both modules. Routes:
   `POST /api/v1/global-users/login` and `/login/complete`.
2. **Password path removed.** The Node's `POST /api/v1/global-users/sessions`
   and Core's `POST …/offline-authorizations` answer `410`. Core's
   password-hash lookup for Nodes (`OfflineCredentials`) is deleted. Migration
   `0012_global_login_assertions.sql` deletes every stored verifier and its
   sessions and drops `password_hash`.
   `TestMigrationDeletesPasswordVerifiersAndTheirSessions` checks this on a
   database migrated to 0011 with a verifier and a session in it.
3. **Offline model (Option A).** The stored Core-signed assertion is the
   grant. It needs a per-Node HttpOnly device credential, applies only on a
   Core outage, has a clock-rollback guard, and is re-verified before use.
   Refresh is shorten-only. Core's refresh now answers with its offline window
   instead of 24 hours.
4. **node-ui.** `/member` ("Sign in with ArkNet", no password field) and
   `/login/callback` (reads the fragment, clears it, completes the login; no
   browser storage). The Vite proxy, live-test nginx and member ingress route
   the new paths.

## Tests

- Agent unit tests: verifier binding, expiry and skew, key rotation, canonical
  encoding, stored-grant recheck, key-set validation, callback registration,
  refresh bound, handler flows (online, replay, foreign Node, unbound browser,
  forged, expired, offline only on outage, device credential, after window,
  tampered grant, rotated key fetched once), configuration and sync.
- Agent PostgreSQL tests against `postgres:18-alpine`: migration, single-use
  completion, replay, pending bound, device bound and replacement, offline
  window and clock rollback, shorten-only refresh, key set replacement.
- Mutation check: 17 of 18 mutations of the new guards made a test fail. The
  one survivor removes the explicit missing-cookie check in `/login/complete`.
  That mutation is equivalent: a missing cookie still finds no pending login
  and is rejected with `401`.
- node-ui: 19 files / 266 tests, including 6 for member sign-in.
- Harness: `scripts/global_login_test.py` (browser flow, cookies, relay) and
  `scripts/mvp_e2e_test.py`.
- Local smoke with compiled Core and Agent binaries passed: online, replay,
  relay capture, offline, after the window, and no secrets in logs.

## Gate

`scripts/check-all.sh --e2e`: 15 passed, 0 failed (mvp-e2e 647 s,
node-upgrade 104 s, product-swarm 869 s).

## Not done

- The live-test VMs were not redeployed. `deploy/live-test` scripts were
  updated (assertion key, callback URL, member ingress, Core-bound logins in
  `initialize-node.py` and `rotate-credentials.py`) but not run. On a fresh
  deployment, `initialize-node.py` now needs the member ingress to be active
  before its login check.
- The design's clock-skew warning from Core's `Date` header is not
  implemented.
