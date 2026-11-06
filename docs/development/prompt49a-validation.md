# Prompt 49A validation — Core login and enrollment abuse

## Scope and verdict

ARKSEC-008's scoped acceptance criteria are satisfied: bounded admission limits,
atomic pending capacity and fixed expiry are tested; signed enrollment is
mandatory; registration receipts do not disclose existing Node state; and the
MVP E2E passes with production limits enabled.

This is validation of the uncommitted `phase20/49a-login-enrollment-abuse`
working tree based on `3953e93`, not Prompt 50's frozen-commit release report.
No live-test deployment or fresh-VM installation was performed.

Policy and operational limitations: [Core abuse controls](../core-abuse-controls.md).

## Evidence

Local logs are under `~/.hermes/cache/scratch/49a/` unless noted otherwise.

| Check | Result | Evidence |
| --- | --- | --- |
| Full gate including E2E | **15 passed, 1 failed** | `full-gate/`; product-swarm startup configuration failure below |
| MVP E2E | PASS, run `arknet-p43-test-4c29549b45a8` | `full-gate/mvp-e2e.log`; enrollment, restart, login, offline expiry, reconnect, relay-only transfer, revocation and isolation passed; known secrets absent from logs |
| Node upgrade | PASS | `full-gate/node-upgrade.log` |
| All component gate stages | PASS | `full-gate/`: scaffold, hygiene, formatting, five Go modules, UI tests/build, harness/deployment tests, real PostgreSQL tests, transfer worker tests |
| Focused HTTP/rate-limit race tests | PASS | `go test -race -count=1 -run TestAbuse .`; shared login budgets, atomic admission, bounded state, IP normalization, backward clock, polling global/per-peer capacity and refill |
| Real PostgreSQL quota/expiry suite | 70 top-level tests plus 11 subtests passed; zero skipped | `~/.hermes/cache/scratch/enrollment-pg-followup/final-restored.log`, parsed Go JSON test events |
| HTTP/limiter mutation checks | 7/7 caught | `mutations/`: peer/global charging, bounded map, API login coverage, service wiring, registration conflict oracle, signature verification |
| Database mutation checks | 5/5 caught | `~/.hermes/cache/scratch/enrollment-pg-followup/mutations.json`: quota threshold, cleanup expiry, identity expiry, expired approval, pending-list expiry |
| Standalone signed enrollment | PASS, run `arknet-enrollment-test-6adcda28debc` | `enrollment-operator-stack.log`; pending admission, conflicting-key receipts, metadata rejection, restart/recreation and repository tests |
| Standalone scoped administration | PASS, run `arknet-admin-test-0f9fd79dd016` | `admin-after-enrollment.log`; decisions/races, persistence, audit, token revocation, removed dev bearer rejection |
| Focused signing/login/retry harness tests | 23 tests passed | `python3 -m unittest scripts.enrollment_signing_test scripts.enrollment_harness_test scripts.login_admission_test scripts.rate_limit_retry_test scripts.mvp_e2e_test` |
| Independent Core and script reviews | PASS; no blocking findings | Separate read-only reviewers; nonblocking additional coverage suggestions only |
| Standalone cleanup | PASS | Parent checked Docker container, network, volume and image names against both exact run prefixes; none remain |

## Gate limitation — product-swarm

`full-gate/product-swarm.log` failed before services started:

> required variable ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE is missing a value

The standalone `check-multi-node-swarm-stack.py` constructs Core's environment
without the relay-verification file now required by the tracker Compose
configuration. It also lacks the relay setup used by the passing MVP E2E.
This is a separate harness integration gap, not a failure of the login or
enrollment checks. It is **not** counted as a passing swarm run or a green full
gate. Migrating that harness's relay topology is left outside this change;
production relay requirements were not weakened to make it pass.

An earlier standalone enrollment attempt failed on an anonymous management
status read (`401`). Its harness now bootstraps the supported operator session,
uses the session cookie through restarts, and passes the real disposable run.
No server authorization was relaxed.

## Operational limits retained

- Rate state is bounded but process-local; restart resets it, and replicas
  multiply the effective budget.
- Forwarding headers are ignored; proxy clients share the socket peer budget.
- Generic enrollment receipts do not promise storage admission at full quota.
- Expiry cleanup is opportunistic, while status/listing/approval enforce expiry
  even before cleanup runs.
- This work does not close the separate ingress-redaction or hostile-production
  validation tasks.
