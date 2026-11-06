# Prompt 49 validation — scoped Core capabilities

Validated 2026-11-06. ARKSEC-005 is closed.

## Delivered

- Per-administrator API tokens, minted only by the Core shell CLI. Only SHA-256
  digests are stored. Explicit permissions, expiry (default 30 days, maximum
  90 days) and immediate revocation replace the shared development bearer.
- Route permissions: `nodes.decide`, `users.admin`, `media.admin`, `relays.read`.
  A route matrix tests every wrong permission and combinations of wrong
  permissions, with repository spies proving denied calls have zero effects.
- Append-only, attributable audit rows record authorized/completed actions,
  wrong-permission denials and token mint/revoke events. An action cannot run
  if its initial audit write fails. Database triggers forbid update/delete/
  truncate. The audit holds IDs and route patterns, not secrets or bodies.
- Per-Node `memberships` and `swarm` capabilities restrict the existing
  enrollment key. Base routes remain available. Both capabilities are the
  compatibility default; `nodes.decide` may narrow them, including to none.
- Harnesses mint short-lived, per-run scoped tokens. The MVP E2E uses separate
  tokens for node decisions, user administration and media administration.
  Live-test initialization and rotation scripts use the Core VM shell to mint
  scoped credentials in memory and revoke them after use, rather than deploy
  a shared bearer. **The live VMs were not redeployed in this validation.**

See [Core administration](../core-administration.md) and D6–D9 in the
[Phase 20 decisions](hermes-phase20-prompts.md).

## Executed verification

- `scripts/check-all.sh`: **13 passed, 0 failed** on the final code tree:
  scaffold, hygiene, gofmt, Core, Agent, tracker, transfer, relay, Node UI,
  harness tests, deploy tests, real PostgreSQL tests and worker tests.
- Harness suite: **83 tests passed**, including local mocked-SSH tests for the
  live-test mint/revoke helpers (no contact with the live VMs).
- `scripts/check-core-admin-stack.py`: disposable Core/Postgres stack passed
  wrong-scope denial with unchanged state, audit attribution, no token in
  responses/logs/database dump, immediate revocation and refusal of removed
  development settings at startup, along with the full real-Postgres suite.
- Core focused race tests and `go vet ./...`: passed.
- Mutation checks: **25 caught** (15 admin/store, 10 Node capability). Invalid
  build trials were rerun separately, not counted as successful catches. The
  relay-route survivor exposed a test that called its guard directly; the
  test now exercises the real route and catches removing the swarm requirement.
- Independent review found a CLI output-error bug. A failing-writer regression
  test reproduced exit 0 with an undelivered live token. The CLI now reports
  failure and attempts to revoke the new token with a fresh bounded context;
  failure to revoke reports only its nonsecret token ID for manual recovery.
- Fresh MVP E2E run **`arknet-p43-test-68e413875194`: 17/17 PASS**. Exact
  machine-readable evidence is committed in
  [prompt49-e2e-results.json](prompt49-e2e-results.json).

The E2E covers enrollment and decisions, Core user creation, pairwise login,
real v2 web-seed and relay transfers, interruption/resume, offline playback,
removed-media retention, revocation, nonenumeration and cross-Node isolation
with the new scoped credentials. Each run removes only its own resources.

## Failures investigated before the final run

- First E2E: 16/17; item 15 selected the first Movie in Jellyfin's library,
  which could be an unrelated fixture after more files were added. A regression
  test with an unrelated movie listed first failed before the fix. Playback now
  selects the exact expected file path and retains size and SHA-256 assertions.
- One gate attempt: 12/13; the unchanged UI Settings test timed out. Both tests
  passed in isolation, and the full gate rerun passed without UI changes.

## Limits

- No browser administrator login or external identity provider was added.
- One Node key remains intentional (D9); capabilities are checked against
  current Core data for each request. Already-issued grants retain their
  existing bounded lifetime; narrowing capabilities is not retroactive grant
  revocation. Use Node revocation for that workflow.
- The initial authorized audit record is mandatory; the post-action completed
  row is best effort. Database-owner privileges can alter triggers; append-only
  enforcement is not protection against a malicious database administrator.
