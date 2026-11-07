# Prompt 51 validation — Production Node UI build

Branch `phase21/51-production-ui`, commits `614a17f` (implementation) and
`519f078` (removal of the pilot's own UI nginx build). Runs on 2026-11-06 AKST.
Logs are local evidence under `~/.hermes/cache/scratch/p51/` and are not
committed.

## Hypothesis

The static build plus a small reverse proxy can replace Vite in the shipped
image without changing the API contract or the LAN HTTPS listener. **Confirmed.**
The proxy is `node-ui/server.mjs`, built only on Node itself with no npm
dependencies, rather than nginx. This keeps the Prompt 45B HTTPS listener, the
read-the-key-then-drop-root sequence and the `start-ui.sh` debug guard
unchanged.

## Acceptance criteria

| Criterion | Result | Evidence |
| --- | --- | --- |
| A test fails before the change: no `vite` package and no `.map` files in the runtime image | **PASS** | `scripts/node_ui_image_test.py` against the old image: 3 failures, 1 error. Found `npm`/`npx`, 1,217 development files (including `.ts` and Vite/TypeScript packages) and no `dist/`; `/media` served the unbuilt page. After the change: `ui-image.log`, 4/4 OK. |
| `npm test -- --run && npm run build` | **PASS** | `ui-tests.log`: 282/282 tests, build OK, 0 source maps in `dist/`. |
| `check-operator-https.py` | **PASS** | `operator-https.log` `EXIT 0`: real Chromium sign-in with password and authenticator code, reload, sign-out, revoked-cookie replay; Secure HttpOnly SameSite=Strict host cookie; scoped cleanup. |
| `check-ingress-redaction.py` with the UI sink still scanned | **PASS** | `ingress-redaction.log` `EXIT 0`: `PASS: five real sinks, verified TLS, forwarding, forced errors, negative control`; the UI proxy-error evidence check still passes. |
| `check-node-upgrade-stack.py` with the new compose hash | **PASS** | `node-upgrade.log` `ALL PASS`. `deploy/node/release.json` carries the new `compose.yaml` SHA-256 `617a1db1…`. |

## What the image now contains

Runtime stage: Node 26 (alpine), `dist/`, `server.mjs` and `start-ui.sh` only.
It contains no `node_modules`, sources, source maps, `npm`, `npx`, corepack or
yarn. The build stage runs `npm ci && npm run build` and is discarded. The
server:

- **Static files:** serves the build from memory, matching exact paths only.
  - Client routes (`/member`, `/login/callback`) get the app page.
  - Missing assets, `.map` files and traversal attempts get 404; non-GET/HEAD
    requests to static paths get 405.
  - Hashed assets are cached as immutable; the page itself is `no-cache` and
    carries a Content-Security-Policy.
- **API proxy:** routes `/api` exactly like the Vite development proxy: member
  and public routes go to the member listener (8082), everything else to
  management (8081). Path, query, method, body and headers pass through
  unchanged, minus hop-by-hop headers.
- **HTTPS listener (8443):** still forwards as the loopback UI
  (`Host: 127.0.0.1:5173`, `X-Forwarded-*`) with the same security headers and
  no HSTS. TLS 1.2 is the minimum.
- **Logging:** never logs a request URL, header or body. An unreachable Agent
  produces the constant `http proxy error: upstream request failed (details
  redacted)` line.
- **Privileges:** reads the TLS key as root, drops to uid/gid 1000 before
  listening, and exits cleanly on SIGTERM.

`deploy/node/compose.yaml` now runs the UI read-only, with a small `/tmp`
tmpfs, every capability dropped except SETUID/SETGID, and
`no-new-privileges`. The image test runs the container under exactly those
constraints.

## Live-test group

`deploy/live-test` now builds and runs the shipped image; its own
`ui.Dockerfile` and `ui-nginx.conf` were equivalent and are removed. Host nginx
still terminates TLS, so the image's own 8443 listener stays off there.

| Step | Result |
| --- | --- |
| `run-live.sh` from a clean `519f078` worktree: build, distribute, deploy (Core, A, B, C) | All `EXIT 0` (`live/summary.txt`, done 23:41:22). |
| `verify-deployment.py` | `DEPLOYMENT_AND_ACCESS_BOUNDARY_PROOF_PASS`. |
| `prove-media.py` | `ALL_THREE_NODE_MEDIA_PROOF_PASS`. |
| `verify-backups.py` | `BACKUP_DECRYPT_AND_RESTORE_PASS` for all four VMs. |
| Spot check on node-a | `/app` = `dist server.mjs start-ui.sh`, no npm, process uid 1000, read-only root, capabilities `SETGID SETUID`, healthy, `/member` 200. |

A scan of the live logs found no grant-like strings, admin tokens or private
keys.

## Notes

- `scripts/check-node-compose.py` fails on `main` too (it expects one UI port,
  but 8443 was added in Prompt 45B). It is not part of the gate and was left
  unchanged.
- The full `check-all.sh --e2e` run is deferred to Prompt 58's frozen tree; the
  checks this prompt names all ran on the implementation commit.
