# Agent / Transfer Service Interface (v1)

Prompt 20 defines a local boundary before BitTorrent implementation. The runnable
`arknet-transfer` implementation is **mock-only**. It simulates progress and
verification outcomes; it never downloads, writes, hashes, or serves media.

## Responsibilities and trust

- **Agent:** ArkNet authorization, enrollment, local operator selection, storage
  reservations, and deciding which catalog object may start or resume.
- **Transfer service:** transfer lifecycle, byte-progress accounting, and reporting
  completion or verification failure. Real byte movement belongs here in a future
  implementation, not in Agent or browser code.
- **UI:** local Agent API only. No transfer token, Core URL, enrollment private key,
  Core admin credential, or direct transfer-service endpoint is sent to browsers.

Agent checks a durable local selection and freshly authorized Core catalog before
`start` or `resume`. Missing or removed objects are rejected. Catalog size must
match the original local reservation; changing remote metadata cannot silently
increase a transfer beyond its reserved size. Pause/cancel remain possible without
Core, including when an object was removed. Selection alone, GET requests, and
Agent startup never start a transfer.

The service token authenticates the **local Agent control channel**, not a global
ArkNet user or Core administrator. It is not a substitute for Agent object
authorization. The service receives only an opaque object ID, size, and optional
v2 info hash. It has no Core client, database credentials, media mounts, or broad
Core administrative credentials. Do not forward enrollment keys or admin tokens.

## Transfer service HTTP API

Every `/api/transfer/v1/` endpoint requires:

```text
Authorization: Bearer <local transfer-service token>
```

The service token must contain 32–256 printable non-whitespace ASCII characters.
Requests and responses use JSON; responses set `Cache-Control: no-store`.
Requests are bounded, strict, and reject unknown fields, duplicate fields,
malformed values, trailing JSON, and query strings. Unsupported methods return
`405`; unknown resources return `404`. Health is the only unauthenticated route:

```text
GET /health -> {"healthy":true,"mock":true}
GET /api/transfer/v1/version -> {"version":"0.0.1-dev","mock":true}
```

### Add authorized object transfer

```text
POST /api/transfer/v1/transfers
```

```json
{
  "object_id": "media_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "size": 1048576,
  "info_hash_v2": ""
}
```

`object_id` is `media_` plus 64 lowercase hex characters, never a path. `size` is a
non-negative integer byte count. `info_hash_v2` is empty (mock placeholder) or 64
lowercase hex characters. No title, URL, arbitrary path, credential, or filesystem
identity is accepted. A real BitTorrent implementation is not authorized by an
empty hash; handling real torrent metadata remains future work.

Add returns `200` with a status. The mock starts in `DOWNLOADING`. Repeating an
identical request returns the existing transfer, including its terminal state,
without restarting it. Reusing an ID with a different size or hash returns `409
conflict`.

### Query / control

| Method | Path under `/api/transfer/v1` | Body | Result |
| --- | --- | --- | --- |
| GET | `/transfers` | none | `{"transfers":[status,...]}` sorted by object ID |
| GET | `/transfers/{object_id}` | none | status |
| POST | `/transfers/{object_id}/pause` | `{}` | status |
| POST | `/transfers/{object_id}/resume` | `{}` | status |
| POST | `/transfers/{object_id}/cancel` | `{}` | status |

Status representation:

```json
{
  "object_id": "media_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "state": "DOWNLOADING",
  "total_bytes": 1048576,
  "completed_bytes": 262144,
  "mock": true,
  "error_code": ""
}
```

`completed_bytes` is between zero and `total_bytes`. States are:

```text
DOWNLOADING <-> PAUSED
DOWNLOADING / PAUSED -> CANCELLED
DOWNLOADING / PAUSED -> COMPLETED
DOWNLOADING / PAUSED -> VERIFICATION_FAILED
```

Repeated same-state actions are idempotent. Incompatible transitions return `409
invalid_transition`. Terminal states cannot resume or restart. A completed status
has `completed_bytes == total_bytes`. Verification failure uses
`error_code: "verification_failed"`; other states use an empty error code.

## Completion and verification-failure reporting

The service engine exposes a small worker-side `Reporter` boundary:

- `ReportProgress(object_id, completed_bytes)`
- `ReportCompletion(object_id)`
- `ReportVerificationFailure(object_id)`

Worker reports update service-owned status. Agent receives completion and failure
reports by polling the status API; no callback listener or Core credential is
required. Status reporting is not an ArkNet authorization decision.

For deterministic mock testing only, the service exposes authenticated worker
simulation routes under `/api/transfer/v1/mock`:

| POST path | Body | Behavior |
| --- | --- | --- |
| `/transfers/{object_id}/progress` | `{"completed_bytes":262144}` | monotonic bounded progress while downloading |
| `/transfers/{object_id}/complete` | `{}` | synthetic completion, bytes set to total |
| `/transfers/{object_id}/verification-failure` | `{}` | synthetic verification failure |

These routes are not exposed by Agent or UI. The mock also advances progress from
elapsed time while downloading and automatically completes after its configured
mock duration. Pause freezes progress; resume continues it. Repeated terminal
reports of the same outcome are idempotent; conflicting reports are rejected.
No actual hash verification is performed, and every status remains `mock:true`.

**Mock completion never changes the durable local selection to `AVAILABLE`.**
The UI explicitly identifies synthetic progress and completion. The mock stores
transfers in memory only: restarting it clears progress. Agent local selections
and storage reservations remain durable, and no transfers automatically restart.

## Error behavior

Service errors use `{"error":"code"}` without internal error text:

| HTTP | Code | Meaning |
| --- | --- | --- |
| 400 | `invalid_request` | malformed ID, size, hash, JSON, fields, or query |
| 401 | `unauthorized` | missing/invalid local service token |
| 404 | `not_found` | no such transfer or endpoint |
| 409 | `conflict` | conflicting add metadata |
| 409 | `invalid_transition` | action/report incompatible with current state |

The Agent client disables redirects and environment proxies, bounds timeouts and
response sizes, checks JSON/content type, validates IDs/states/byte counts, and
rejects non-mock or inconsistent responses. Errors are sanitized; tokens, endpoint
URLs, service response bodies, and transport details are never returned to UI.

## Agent / UI endpoints

```text
GET  /api/v1/transfers
POST /api/v1/media/{media_id}/transfer/start
POST /api/v1/media/{media_id}/transfer/pause
POST /api/v1/media/{media_id}/transfer/resume
POST /api/v1/media/{media_id}/transfer/cancel
```

Actions require `Content-Type: application/json` and `{}`; user-provided transfer
metadata is rejected. Existing local cross-origin mutation protection applies.
Agent list responses contain only locally selected objects:

```json
{"enabled":false,"available":false,"transfers":[]}
```

`enabled` indicates configuration; `available` indicates a successful service
read. Transfer unavailability does not break catalog or queue functionality.
A successful action returns status. Agent errors include:

- `400 invalid_request`
- `404 media_not_found` / `transfer_not_found`
- `409 media_removed` / `transfer_conflict`
- `503 catalog_unavailable` / `local_media_unavailable` / `transfer_unavailable`

The UI polls sequentially while the Media view is mounted and displays synthetic
byte progress and terminal reports separately from the real local media state.
The transfer endpoints on the local Agent management API require a Node-local
operator session. Member bearer credentials do not grant management access.
The shipped UI exposes this plane through the self-signed LAN HTTPS listener;
plain HTTP is host-loopback only.

## Run the mock

Standalone service:

```sh
export ARKNET_TRANSFER_TOKEN="$(openssl rand -hex 32)"
cd transfer
go run .
```

Service defaults: `ARKNET_TRANSFER_ADDR=127.0.0.1:8084`,
`ARKNET_TRANSFER_MOCK_DURATION=10s`. The token is required and has no built-in
value. For a standalone Agent, set `ARKNET_TRANSFER_URL=http://127.0.0.1:8084` and
the same local `ARKNET_TRANSFER_TOKEN`; both unset disables transfer integration.
Both configured values are validated at Agent startup. Plain HTTP is accepted only
for loopback addresses and the exact Compose sidecar service name
`arknet-transfer`; every other hostname and non-loopback IP address requires HTTPS,
so an endpoint typo cannot send the service token over remote plaintext HTTP.

For the Node development Compose stack, explicitly opt into the mock overlay:

```sh
cd deploy/node
export ARKNET_TRANSFER_TOKEN="$(openssl rand -hex 32)"
docker compose -f compose.yaml -f compose.transfer-mock.yaml up --build -d --wait
```

The overlay adds a private internal `transfer-control` network shared only by
Agent and transfer. Transfer has no published host port, media mounts, PostgreSQL
connection, or Core credentials. Use the same overlay files when stopping or
recreating this stack. Do not put the service token in browser environment files
or commit it. Normal four-service Compose deployment is unchanged without the
overlay.

## Verification and deliberate exclusions

```sh
# Within each Go module: transfer/, node-agent/, and core/
go test -race -count=1 ./...
go vet ./...
# Within node-ui/
npm test
npm run build
# From repository root; disposable resources only
python3 scripts/check-transfer-stack.py
```

The live check exercises real Core authorization, durable local selections, the
Agent HTTP client, service isolation, progress, pause/resume/cancel, completion,
and verification-failure reporting. Service tests also verify a compiled process,
real listener, and SIGTERM shutdown.

No BitTorrent library, actual transfer, torrent generation, peer discovery,
DHT/PEX/local discovery, tracker, relay, web seed, peer serving, media path, or
Jellyfin library import is implemented in this prompt. Those remain separate
future work rather than being implied by simulated progress.
