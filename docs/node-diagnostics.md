# Node Diagnostics

The **Diagnostics** page in the Node UI explains what works, what doesn't,
and what to do. It is aimed at operators, so it gives plain-language causes,
such as *"Federation Offline — Local services are still operating."*, rather
than network error strings. No SSH or shell is needed.

## What it checks

| Check | Source | Typical explanations |
|---|---|---|
| ArkNet version | Agent build plus the verified release manifest | Release and channel, or a warning that the release can't be confirmed |
| ArkNet Agent | The Agent answering the request | If the UI can't reach the Agent at all: *Node Agent Offline* |
| PostgreSQL | Agent `Ping`, bounded to 2 s | *Local Database Unavailable* |
| Jellyfin | Agent health probe | *Media Server Offline — members cannot stream from this Node* |
| Core connectivity | Live probe of Core `/health`, plus the Node state, enrollment state and last heartbeat | Address not found, connection refused, no response, no route, certificate rejected, Core-side error; waiting for approval; revoked or quarantined |
| Private tracker | Live probe of the tracker origin (transfer overlay only) | Any HTTP answer counts as reachable, because the tracker requires swarm authorization. Same network causes as Core. |
| Tailscale | Unprivileged DNS query to Tailscale's on-host resolver `100.100.100.100` | Informational only: no ArkNet release depends on Tailscale yet |
| Storage capacity | `statfs` on the media volume, or Docker's data filesystem without transfers | Warning below 10 % free, problem below 5 % |
| ArkNet media storage | Reserved bytes against the allocation | Disabled (0 GiB), nearly full (≥ 95 %) |
| Active transfers | Transfer service list | Downloading, paused or failed counts; service not responding |

The headline puts the most important problem first: local database, then
media server, then federation, then revocation, then other items, then
waiting for approval.

## How it works

`GET /api/v1/diagnostics` on the Agent is read-only.

- It runs every check concurrently. Network probes are bounded to 4 s and
  database calls to 2 s, so one hung dependency can't stall the page.
- It returns machine-readable status codes. The UI (`diagnosticsText.ts`)
  turns them into the explanation and the "What to do" text.
- Network failures are classified from Go error types: DNS, connection
  refused, timeout, no route, TLS verification, HTTP 5xx.
- Probes send no credentials, Node identity or redirects, and the response
  shows only hosts, never paths or secrets.
- The page refreshes every 30 s and has a **Run checks again** button.

The transfer overlay now also passes `ARKNET_TRANSFER_TRACKER_ORIGIN` to the
Agent, which uses it only for this check. The release manifest digest for
`compose.transfer.yaml` was updated.

## Verification (2026-10-25)

- Agent tests cover:
  - every check healthy, failing and not configured;
  - a hung probe still answering within the bound;
  - real classification of 404 / 503 / refused / self-signed TLS /
    `.invalid` DNS / timeout;
  - no credentials sent.

  Seven mutations were each caught: DNS and refused classification, the
  tracker HTTP rule, the database bound, error-coded transfers, the statfs
  path, and the credentials guard.
- UI tests: 15 cases covering every required check, each Core failure type
  in plain words, headlines, the unreachable Agent, malformed responses and
  contact-history wording.
- Live, on the real Node stack with the transfer overlay and a host Core,
  using only the browser page:

  | Fault introduced | Headline shown | Detail |
  |---|---|---|
  | none (pending enrollment, unresolvable tracker) | *1 item needs attention* | *Address not found* for the tracker; *Waiting for approval* for Core |
  | Core stopped | *Federation Offline — Local services are still operating.* | Core *connection refused*, with the action |
  | Jellyfin stopped | *Media Server Offline — members cannot stream from this Node.* | — |
  | PostgreSQL stopped | *Local Database Unavailable* | Core *Reachable; Node state unknown* |
  | Agent stopped | *Node Agent Offline* | The UI proxy returned 502 and the page explained it |

  All checks recovered when services returned. Tailscale was correctly
  *Detected*: this host runs Tailscale and `100.100.100.100` routes through
  it.

## Limits

- **Tailscale detection** relies on Tailscale's `100.100.100.100` resolver
  being routable from the Agent's container. Tailscale setups that don't
  route it show *Not detected*. That is harmless while Tailscale is not
  required.
- **Recovery is not one-click:** the advice for local service failures is to
  re-run the installer, which restarts and re-validates the stack. Restarting
  services from the UI would need Docker control in the Agent, which it
  deliberately does not have.
- **Reaching the UI:** the UI stays on loopback (reach it over an SSH tunnel)
  until a login exists for the management UI.
