# ArkNet Node UI

React + TypeScript local management UI with a read-only operations dashboard for the local Node Agent. It
separates the health of services on this machine from ArkNet federation
connectivity, so an operator can immediately tell “local node running, Core
offline” from a local failure.

The interface is deliberately data-dense and uses semantic HTML, text plus color
status indicators, responsive layouts, system fonts, and local assets only. It
uses no third-party assets or remote credential service. Operator setup and
sign-in use Node-local password plus TOTP (or a one-time recovery code). The
session is an HttpOnly cookie; credentials and operator data are never written
to localStorage or sessionStorage. Its Settings page contains the local Peer
Serving opt-in.

## Run locally

Use npm and a supported Node.js version: 22.22.2+, 24.15.0+, or 26+ (the tooling
supports these version branches). Go 1.25+ and a configured PostgreSQL database
are needed to run the Agent standalone. See [Agent database configuration](../node-agent/README.md).
For the easiest workflow, start the Node Compose stack, then run local Vite with
`ARKNET_AGENT_URL=http://127.0.0.1:8082 ARKNET_MEMBER_URL=http://127.0.0.1:8086 npm run dev`.

If using a standalone Agent with a configured database, start it in one terminal
from the repository root:

```sh
cd node-agent
go run .
```

In another terminal:

```sh
cd node-ui
npm ci
npm run dev
```

Open **http://127.0.0.1:5173/**. No Core service is required. The UI development
server binds to loopback. It proxies login and session routes to the Agent's
member listener (`http://127.0.0.1:8082` by default) and the rest of `/api` to the
management listener (`http://127.0.0.1:8081` by default).
Global members sign in at `/member`: the button sends them to ArkNet Core,
which returns them to `/login/callback` (configure that URL on the Agent with
`ARKNET_GLOBAL_LOGIN_CALLBACK_URLS`). No global password field exists in this UI.
The browser fetches `/api/v1/status` on its own origin; the Agent needs no CORS
changes and is not exposed on additional interfaces.

To use other local Agent addresses, configure the server-side proxy targets:

```sh
ARKNET_AGENT_URL=http://127.0.0.1:9081 ARKNET_MEMBER_URL=http://127.0.0.1:9082 npm run dev
```

`ARKNET_AGENT_URL` (management) and `ARKNET_MEMBER_URL` (member) may also be set in an ignored `.env.local` file. Neither is a
browser credential or Core address. Restart Vite after changing proxy settings.
Never place secrets in `VITE_*` variables, which are exposed to browser code.

## Operator setup and HTTPS

Create the first operator using `sudo arknet-node operator setup-code` on an
installed Node (or `docker compose exec -T arknet-agent arknet-agent operator
setup-code` in the development stack). In the UI choose **Set up operator**,
enroll an authenticator and save the displayed recovery codes. After setup,
use **Sign in** and **Sign out** in the header. The Agent enforces permissions
independently of the UI; anonymous/member calls to management routes return 401.

The Docker image starts `node server.mjs`: Vite serves port 5173, and the HTTPS
listener serves 8443 when `/etc/arknet/tls/cert.pem` and `key.pem` exist.
`install.sh` and upgrades generate that certificate; see
[Node installation](../docs/node-installation.md). Plain HTTP is published only
on loopback; HTTPS is published to the LAN. Standalone HTTPS development can use
`ARKNET_UI_TLS_CERT` and `ARKNET_UI_TLS_KEY` with `node server.mjs`.

Browser acceptance against a disposable real Agent/Postgres/UI stack:

```sh
# From the repository root; Playwright and its Chromium must be installed.
PLAYWRIGHT_MODULE=/absolute/path/to/node_modules/playwright \
  python3 scripts/check-operator-https.py
```

The harness discovers a LAN address (or accepts `--lan-address`), confirms a
positive loopback HTTP request and refused LAN HTTP connection, then signs in,
reloads, signs out and replays the revoked cookie over HTTPS. It checks
Secure/HttpOnly/SameSite flags and empty localStorage/sessionStorage, logs no
credentials or cookie values, and removes its scoped resources.

## Media selections

Choose **Media** in the local navigation to see catalog titles, sizes, local
states, allocation, and reserved bytes. **Make Available Locally** queues an
eligible selection only; it does not start a transfer. A transfer requires a
separate explicit operator action. Core must be online and the allocation positive with enough remaining
space. A zero allocation blocks even zero-byte objects. The Agent remains
authoritative for allocation checks.

After selection, the UI refreshes the catalog and reserved bytes. If a selection
or refresh fails, controls stay locked until **Retry media refresh** succeeds;
retry only reads state and never repeats a POST. Saved selections remain visible
while Core is offline. Media requests use same-origin `/api/v1/media`, bypass
browser caching, and are canceled on navigation or after a 10-second timeout.
See [Node media selection](../docs/node-media-selection.md) for usage and configuration.

### Transfer progress

Media accepts both real (`mock: false`) and synthetic (`mock: true`) status records
from the Agent's transfer endpoint. Real progress and pause, resume, and cancel
actions use **Transfer** labels without mock wording. A completed real status is
reported conservatively as a verified transfer; it does not make the UI claim that
the title is locally available or available to Jellyfin. Only the media catalog's
`AVAILABLE` local state is authoritative for local availability.

Synthetic progress retains explicit **Mock** labels and warnings that no media was
downloaded or verified. Starting any transfer is a separate operator action after
selection; selection, page loads, and Agent restarts never start one automatically.
The browser talks only to the Agent and never receives the local transfer-service
token or Core credentials.
See [Transfer service interface](../docs/transfer-service-interface.md).

## Peer Serving

Choose **Settings** to view or change **Peer Serving**. It defaults to off and is
persisted by the local Node Agent. Turning it on allows Core to consider known
local replicas as peer sources; turning it off excludes this Node from future
source selection after the updated heartbeat is accepted. Receiving media and
the Core web-seed fallback remain available while it is off. See
[Opt-In Peer Serving](../docs/peer-serving.md).

## Status contract

The dashboard runtime-validates the complete `/api/v1/status` response:

```ts
{
  node_id: string;                         // opaque, public identifier
  version: string;
  state: string;
  state_changed_at: string;                // RFC 3339
  enrollment_state: string;
  core_connectivity: string;
  last_successful_core_contact_at: string | null; // RFC 3339 or null
  database_healthy: boolean;
  agent_healthy: boolean;
  jellyfin_healthy: boolean | null;
}
```

The full Node ID remains visible and selectable. Timestamps are rendered in UTC
with their exact source value retained in the semantic `<time datetime>`
attribute. A null last contact is displayed as **Never**.

Known Node, enrollment, and Core machine codes are converted to operator-friendly
labels. Unknown future strings are preserved as `Unknown (MACHINE_VALUE)` rather
than hidden or treated as trusted markup.

## Dashboard and failure behavior

The summary independently reports **Local services** and **Federation**. Agent and
database health determine the local summary; Core connectivity determines the
federation summary. A Core outage therefore does not imply that local services
are down.

The first request has a 10-second timeout. Subsequent requests are scheduled five
seconds after the previous request settles, preventing overlapping polls and
loading-screen flicker. Requests and timers are canceled when the view unmounts.
A manual retry starts a fresh request.

Operator errors are translated into distinct, actionable messages:

- proxy `502` / Agent unavailable: check the Agent process and proxy target;
- Agent `503` / local state database unavailable: check PostgreSQL and Agent
  database access;
- malformed or contract-invalid payload: check UI/Agent version compatibility;
- timeout: check whether the Agent is stopped or overloaded;
- browser/network failure: check the Agent, local network, and proxy target.

No machine status is fabricated when validation or transport fails.

## Structure

- `src/main.tsx`: React entry point.
- `src/App.tsx`: application shell.
- `src/pages/Dashboard.tsx`: polling, summaries, details, and error UI.
- `src/pages/Settings.tsx`: local Peer Serving control.
- `src/api/status.ts`: typed, runtime-validated Agent status client.
- `src/api/status.test.ts`: API contract and transport classification tests.
- `src/pages/Dashboard.test.tsx`: render, accessibility, error, and poll tests.
- `src/styles.css`: responsive, local-only dashboard styling.
- `vite.config.ts`: loopback development proxy and test configuration.

## Test and build

```sh
npm test
npm run build
```

The build type-checks all TypeScript and produces ignored static assets in
`dist/`. Exact package versions and `package-lock.json` make `npm ci`
reproducible.

`npm run preview` previews the static build and inherits Vite's local Agent proxy
configuration; it is not a production serving solution. In production, the
serving layer must route same-origin `/api/v1/*` requests to the local Agent.
For the local container workflow, see the [Node development Compose stack](../deploy/node/README.md).
