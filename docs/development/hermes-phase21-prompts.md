# ArkNet Hermes Prompt Pack — Phase 21: Production Readiness

## How to use this pack

Phase 20 ended on frozen commit `b232f46` with every tracked ARKSEC item closed
and every acceptance check passing (see the
[validation report](../mvp-validation-report.md)). The verdict was still
**not ready for hostile multi-operator production**. The remaining gaps are
not security-tracker items; they are release, operations and resilience
basics. This phase closes them in dependency order.

Give Hermes **one prompt at a time, in order**. Each prompt is self-contained.
A prompt is finished only when its stated tests have actually run and passed.

### Status snapshot (2026-11-06)

Already proven on `b232f46`:

- functional MVP (E2E items 1–20), whole-stack upgrade and rollback, fresh-VM
  install, product swarm over the private relay;
- ARKSEC-001 to 009 closed or fixed;
- live-test pilot on the same commit: deployment, media, backup/restore and
  reboot verifiers all `EXIT 0`.

Known gaps, confirmed in the repository:

- **Version and release.** Every component is `0.0.1-dev` on the
  `development` channel (`VERSION`, `deploy/node/release.json`). Compose files
  reference local tags such as `arknet-core:0.0.1-dev`; there is no registry,
  no published image and no signed release. The shipped Node and Core compose
  files use the floating `postgres:18-alpine` (only the live pilot pins a
  digest).
- **Node UI runs Vite's dev server.** `node-ui/Dockerfile` runs `start-ui.sh`,
  which starts `server.mjs`, which calls Vite's `createServer`. The live pilot
  avoids this with a separate static build (`deploy/live-test/ui.Dockerfile`);
  the shipped image does not.
- **No CI.** `scripts/check-all.sh` runs only on the operator Mac. There is no
  git remote and nothing is pushed. D1 deferred a self-hosted runner.
- **No observability.** No service exposes metrics; there is no alerting or
  log retention policy.
- **Single instances.** One Core, one tracker, one relay; Core's login and
  enrollment limits live in one process. In the pilot, Core is also the only
  DNS server.
- **Certificates.** The pilot uses a private CA over a VPN. The supported TLS
  ingress expects operator-supplied certificates; nothing issues or renews them.
- **Disaster recovery.** Backups are pulled only to the operator Mac; the
  Mac's CA key and backup age identities have no off-site copy.
- **Residual risks accepted in Phase 20.** A malicious Node keeps grants it
  already holds for up to 5 minutes; rate limits reset on restart and are
  shared behind a proxy; no Agent clock-skew warning; no independent security
  review.
- **Never measured.** Load/capacity, dependency and image vulnerabilities,
  a full Core-outage restore drill, incident runbooks.

### Rules for every prompt

All [Phase 20 rules](hermes-phase20-prompts.md#how-to-use-this-pack) apply. In
addition:

- One branch per prompt (`phase21/<id>-<slug>`); fast-forward `main`; never
  push unless the user asks.
- Never print secrets, tokens, grant URLs or private keys; generate test
  credentials at runtime. Registry, DNS, CA and monitoring credentials come
  from the user's vault or an operator-run shell, never from chat.
- Do not change live DNS, Cloudflare, Headscale, trust stores or the live-test
  VMs unless the prompt says so; when it does, keep a timed rollback.
- Report the `EXIT` line of each check log. Runtime changes invalidate
  `b232f46` evidence; Prompt 58 reruns everything on a new frozen commit.
- Design prompts produce a document only, list open questions and stop.

## Decisions needed from the user

Each has a recommended default. A prompt that depends on an undecided item
must stop and ask, not assume.

| ID | Decision | Recommended default | Needed by |
| --- | --- | --- | --- |
| D11 | Image registry and naming | A self-hosted OCI registry you already operate (or GHCR if public reuse is intended), `<registry>/arknet/<component>:<version>`, always deployed by digest. | 51A |
| D12 | Release signing | Sign the release manifest and image digests with a dedicated offline Ed25519 key (minisign or cosign key mode); the Agent verifies the manifest before upgrade. No keyless/transparency-log dependency. | 51A |
| D13 | CI runner | A self-hosted runner on infrastructure you own that can run Docker and Lima; runs `check-all.sh` on every branch and `--e2e --vm` nightly and before tags. | 51B |
| D14 | Monitoring stack | Prometheus-format metrics scraped by your existing self-hosted monitoring; alerts to Gotify or Discord; 14-day log retention on hosts. No hosted SaaS. | 52 |
| D15 | Public exposure model | Core, tracker and relay behind your owned ingress (Traefik/Cloudflare Tunnel) with publicly trusted certificates; relay needs a raw TLS/TCP path, so decide whether it bypasses the tunnel. | 53 |
| D16 | Certificate issuance | ACME (Let's Encrypt) DNS-01 through your DNS provider for Core/tracker; relay keeps its SPKI-pinned self-signed certificate. | 53A |
| D17 | Core availability target | Single Core instance with documented RTO ≤ 1 h and RPO ≤ 24 h for the first release; HA deferred. Rate-limit state stays process-local. | 54 |
| D18 | Off-site DR copy | Encrypted copy of the CA signing key, backup age identities and release-signing key in a second location you control (offline media or a separate vault). | 54A |
| D19 | Capacity targets | First release: 50 Nodes, 500 global users, 20 concurrent transfers per relay at ≥ 1 MiB/s each, on 2 vCPU / 4 GiB Core. | 55 |
| D20 | Residual grant window | Keep 5-minute grants, but shorten tracker/relay grants to 2 minutes if measured load allows; document the bound. | 56A |

---

# PHASE 21A — Release Foundation

## Prompt 51 — Production Node UI Build

Context: the shipped `node-ui` image runs Vite's development server through
`server.mjs` (`createServer` from `vite`). Prompt 49B added proxy-log redaction
and a debug-environment guard around it. The live pilot already serves a
static build through nginx (`deploy/live-test/ui.Dockerfile`,
`deploy/live-test/ui-nginx.conf`). Hypothesis to confirm: the static build plus
a small reverse proxy can replace Vite in the shipped image without changing
the API contract or the LAN HTTPS listener (Prompt 45B).

Task:

1. Build the UI at image build time (`npm run build`) and serve `dist/` from a
   production server: either nginx (as in the pilot) or a minimal Node
   built-in server already used by `server.mjs` for HTTPS. Do not ship Vite,
   dev dependencies or source maps in the runtime image.
2. Keep: `/api` proxied to the Agent without path rewriting, the 5173 loopback
   listener, the 8443 LAN HTTPS listener with the self-signed certificate, the
   security headers, grant-safe logging (no request URLs in access or error
   logs), and dropping privileges after reading the key.
3. Update `deploy/node/compose.yaml` and its sha256 in
   `deploy/node/release.json`, `install.sh` if needed, and the upgrade check.
4. Make `deploy/live-test` use the shipped image instead of its own
   `ui.Dockerfile` if the result is equivalent; otherwise document why not.

Acceptance criteria:

- A test fails before the change by asserting the runtime image contains no
  `vite` package and no `.map` files; it passes after.
- `npm test -- --run && npm run build` pass; `check-operator-https.py` passes
  (real Chromium login/logout over LAN HTTPS).
- `check-ingress-redaction.py` passes with the UI sink still scanned.
- `check-node-upgrade-stack.py` passes with the new compose hash.

## Prompt 51A — Versioned, Signed Releases From a Registry

Context: compose files reference local `:0.0.1-dev` tags; images are built on
each host. Depends on D11 and D12.

Task:

1. Add a release script that builds every ArkNet image (core, tracker, relay,
   agent, transfer, ui) from a clean checkout of a tag, pushes it to the D11
   registry, and records each digest.
2. Generate `release.json` with digest-pinned images (including PostgreSQL and
   Jellyfin) and sign it with the D12 key. The signing key is used from an
   operator shell; the script never reads it from the repository.
3. The Agent's update discovery and `arknet-node upgrade` verify the
   signature against a public key shipped with the install before pulling.
   An unsigned or wrongly signed manifest is refused with no change.
4. Pin `postgres:18-alpine` by digest in `deploy/core/compose.yaml` and
   `deploy/node/compose.yaml`.
5. Version scheme: drop `-dev` for tagged releases; document it in
   `docs/development/version-control.md`.

Acceptance criteria:

- Unit tests: tampered manifest, wrong key and unsigned manifest are refused;
  a valid one is accepted.
- `check-node-upgrade-stack.py` upgrades A → B from signed, digest-pinned
  manifests and rejects a tampered B with no change (new negative case).
- `check-node-install-vm.py` installs from the registry on a fresh VM.
- No compose file references an unpinned image (enforced by a test).

## Prompt 51B — Continuous Integration Runner

Context: the gate runs only on the operator Mac. Depends on D13.

Task:

1. Add runner configuration for D13 that runs `scripts/check-all.sh` on every
   branch push and `--e2e --vm` nightly and before release tags, keeping step
   logs and diagnostics as artifacts (already sanitized by the harnesses).
2. Add a git remote only if the user provides one; never push without consent.
3. Document runner setup, required host capabilities (Docker, Lima, OpenSSL 3,
   Playwright Chromium) and how secrets are absent from the runner.

Acceptance criteria:

- One real runner execution of the component gate ends `EXIT 0`, with its log
  linked from the prompt's validation note.
- A deliberately failing branch shows a failed run with the failing step.

---

# PHASE 21B — Observability

## Prompt 52 — Design: Metrics, Alerts and Log Retention

Context: no ArkNet service exposes metrics. Logs are JSON on stdout/stderr and
already avoid grants and secrets. Depends on D14.

Task: write `docs/observability.md`:

1. Per service (Core, tracker, relay, Agent, transfer), the metrics worth
   exposing: health, request counts by fixed route label and status, rate-limit
   rejections, enrollment queue size, relay sessions and bytes, transfer states,
   heartbeat age, backup age.
2. Privacy rules: no Node IDs, member IDs, usernames, IP addresses, object IDs
   or grant material as label values; fixed label sets only.
3. Where metrics listen (loopback or an internal network, never public) and
   how the D14 stack scrapes them.
4. The alert list with thresholds and runbook links (Prompt 57).
5. Log retention and rotation per host.

Acceptance criteria: a document with open questions; no code.

## Prompt 52A — Metrics Endpoints

Context: depends on the approved Prompt 52 design.

Task:

1. Add the designed metrics to each service, on the designed listener, using
   the Go standard library or the smallest dependency the design approved.
2. Add a test per service that scrapes the endpoint and asserts the exact
   label sets, and a sentinel test that no identifier or grant appears in any
   label or value.
3. Wire scraping into the live-test pilot only if the user approves.

Acceptance criteria:

- Tests fail before and pass after; metrics listeners are absent from host
  port bindings (compose render test).
- `check-ingress-redaction.py` and the MVP E2E still pass.

## Prompt 52B — Alerting and Log Retention

Context: depends on 52A.

Task:

1. Add alert rules for the Prompt 52 list (service down, Core unreachable from
   Nodes, relay down, backup older than 26 h, certificate expiry < 14 days,
   disk > 85 %, rate-limit rejection spikes).
2. Configure log rotation and retention on Core and Node hosts and in the
   live-test tooling.
3. Exercise each alert once in a disposable environment.

Acceptance criteria: a log of each alert firing and resolving in a test
environment; retention verified on a live-test VM.

---

# PHASE 21C — Exposure and Certificates

## Prompt 53 — Design: Public Exposure of Core, Tracker and Relay

Context: the pilot is VPN-only with a private CA. The relay needs raw TLS to
port 7443 and authenticates by SPKI pin; Core and the tracker are HTTPS.
Depends on D15.

Task: write `docs/public-exposure.md` covering ingress topology per service,
which paths go through the owned ingress and which need TCP passthrough,
firewall rules, DDoS and rate-limit placement (forwarding headers are ignored by
Core today), how grant-safe logging is preserved at every proxy hop, and
rollback. List open questions.

Acceptance criteria: a document only.

## Prompt 53A — Automated Certificates

Context: depends on 53 and D16.

Task:

1. Issue and renew publicly trusted certificates for Core and the tracker
   (ACME DNS-01), installed where the supported TLS ingress reads them, with
   reload on renewal.
2. Keep the relay's pinned certificate; document its rotation (new pin
   published through Core's signed relay list before the old one is retired).
3. Alert on expiry (52B).

Acceptance criteria: a staging-CA issuance and renewal run in a disposable
environment; `check-ingress-redaction.py` passes against the issued chain.

---

# PHASE 21D — Resilience and Recovery

## Prompt 54 — Core Availability and Restore Plan

Context: one Core, one tracker, one relay. Depends on D17.

Task: write `docs/core-availability.md`: failure modes and their effect on
Nodes (offline playback and cached login keep working; transfers stop), the
RTO/RPO targets, the restore procedure from encrypted backups, and what HA
would require later. Remove Core as the only pilot DNS server or document the
mitigation.

Acceptance criteria: a document with open questions.

## Prompt 54A — Off-Site Disaster-Recovery Copies

Context: CA signing key, backup age identities and (after 51A) the release key
exist only on the operator Mac. Depends on D18.

Task: add a documented, scripted procedure that makes an encrypted second copy
in the D18 location and verifies it can decrypt a current backup archive,
without printing or transmitting key material in clear text.

Acceptance criteria: a real verification run that decrypts a current backup
using only the second copy.

## Prompt 54B — Full Core Outage Restore Drill

Context: depends on 54 and 54A.

Task: in a disposable environment, destroy Core's database and volumes, restore
from the latest off-site backup, and prove Nodes reconnect with their existing
enrollment, memberships and grants policy, within the RTO.

Acceptance criteria: a drill log with timestamps showing restore within RTO,
Nodes CONNECTED without re-enrollment, and the MVP E2E smoke items 2, 9 and 14
passing afterwards.

---

# PHASE 21E — Capacity and Supply Chain

## Prompt 55 — Load and Capacity Test

Context: never measured. Depends on D19.

Task: add a load harness that simulates the D19 Node and user counts against
Core (heartbeats, catalog, logins within rate limits, grant issuance) and
relay sessions with real transfers; record CPU, memory, latency percentiles
and error rates; find the first limit.

Acceptance criteria: a results file and report against D19 targets, with
every failure classified (product, harness, environment) and any product bug
given a regression test.

## Prompt 55A — Dependency and Image Vulnerability Scanning

Context: no scanning exists.

Task: add `govulncheck` for every Go module, `npm audit` (production
dependencies) for `node-ui`, and an image scanner for every shipped image to
the gate, failing on fixable high/critical findings with an allowlist that
requires a reason and expiry.

Acceptance criteria: the gate step runs and passes, or each finding is fixed
or allowlisted with a reason; a test proves an expired allowlist entry fails.

---

# PHASE 21F — Residual Security Risks

## Prompt 56 — Agent Clock-Skew Warning

Context: assertion, grant and token checks depend on clocks; there is no
warning when a Node's clock drifts. Core responses carry a `Date` header.

Task: compare Core's `Date` with local time on heartbeat; expose skew in status
and metrics; warn above 30 s and mark DEGRADED above the smallest token
leeway.

Acceptance criteria: unit tests with a fake clock; status shows the skew; no
change to authorization decisions.

## Prompt 56A — Bound the Malicious-Node Grant Window

Context: a malicious Node keeps already-issued grants for up to 5 minutes
after revocation (documented in `docs/node-revocation.md`). Core already
pushes revocation to the relay, which closes live sessions. Depends on D20.

Task: measure whether shorter tracker/relay grants raise load meaningfully
(Prompt 55 harness); apply D20; make the relay and tracker reject grants for a
pushed-revoked Node immediately, not only at expiry, where they do not already.

Acceptance criteria: an E2E revocation case shows a revoked Node's held grant
refused by tracker and relay within one push interval; the documented window is
updated with the measured bound.

## Prompt 56B — Rate Limits Behind a Trusted Proxy

Context: Core ignores forwarding headers by design, so all clients behind one
proxy share a budget. Depends on 53.

Task: add an explicit trusted-proxy allowlist (CIDRs) so Core uses the
forwarded client address only from those peers; everything else keeps using
the socket peer. Keep the bounded-map, fail-closed behavior.

Acceptance criteria: tests for spoofed headers from an untrusted peer (ignored),
from a trusted peer (honored), and malformed chains (rejected or socket peer).

---

# PHASE 21G — Operations and Validation

## Prompt 57 — Operator Runbooks

Task: write `docs/runbooks/` for each Prompt 52 alert, Core restore (54),
relay certificate/pin rotation (53A), Node revocation, credential rotation,
release and rollback (51A), and incident communication. Each runbook names its
verification command.

Acceptance criteria: every alert links to a runbook; a doc-link test passes.

## Prompt 57A — Independent Security Review Package

Context: no independent review has been done. This prompt prepares one; it
does not replace it.

Task: write `docs/security-review-scope.md`: architecture and trust boundaries,
threat model, the closed ARKSEC items with evidence, residual risks, in-scope
components and environments, test accounts to be minted at review time, and
rules of engagement. List open questions for the user.

Acceptance criteria: a document only. ArkNet stays "not ready for hostile
production" until the user records the external review's outcome.

## Prompt 58 — Phase 21 Validation Report

Task:

1. On one frozen, committed tree, run `check-all.sh --e2e --vm`, every new
   check added in this phase, the load test, the restore drill and the
   live-test verifiers including reboots.
2. Update `docs/mvp-validation-report.md`.
3. Verdict: is ArkNet ready for hostile multi-operator production? The answer
   is no while the external review (57A) is unrecorded or any check fails.

Acceptance criteria:

- Every PASS cites a log from the frozen tree, identified by commit hash.
- Each Phase 21 gap from the status snapshot is closed with evidence or listed
  as open with a reason.
