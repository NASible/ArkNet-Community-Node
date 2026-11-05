# Transfer service

Standalone Go 1.25 module (`arknet/transfer`) with a local authenticated HTTP API.
The production runner launches the separate Python/libtorrent worker; the Go
`engine` package remains an in-memory development mock used by the explicit mock
deployment and tests. Version `0.0.1-dev`.

The real worker accepts only private v2 metadata matching the expected full hash
and size, disables DHT/PEX/LSD, and confines writes to its staging directory. It
fetches metadata through a short-lived Core `torrent_url` and uses the matching
`web_seed_url` after validating both exact origins, opaque object IDs, and canonical
token paths. Both URLs stay in memory and are removed from manifests and resume data.

## Run

```sh
# Supply a distinct local service secret through your environment/secret manager.
export ARKNET_TRANSFER_TOKEN='<32-to-256-printable-ASCII-nonwhitespace-characters>'
go build -o /tmp/arknet-transfer .
/tmp/arknet-transfer
```

| Environment | Default / constraints |
| --- | --- |
| `ARKNET_TRANSFER_TOKEN` | Required; 32–256 ASCII bytes `!` through `~`, no whitespace/control/CR/LF. Never logged or returned. |
| `ARKNET_TRANSFER_ADDR` | `127.0.0.1:8084`; numeric IP and port 0–65535. Port 0 chooses an ephemeral port; DNS hostnames are rejected (no outbound DNS). |
| `ARKNET_TRANSFER_TRACKER_URL` | Required authorized private-tracker URL. |
| `ARKNET_TRANSFER_TRACKER_ALLOW_HTTP` | `false`; development-only HTTP opt-in. |
| `ARKNET_TRANSFER_WEB_SEED_ORIGIN` | Optional trusted Core origin, with no path/query/fragment. If unset, web-seed URLs are rejected. |
| `ARKNET_TRANSFER_WEB_SEED_ALLOW_HTTP` | `false`; explicit local-development HTTP opt-in. It disables libtorrent's session-wide SSRF guard, so ArkNet compensates by accepting only the configured origin and canonical Core object/token path. Invalid without an origin; never enable for an untrusted endpoint. |
| `ARKNET_TRANSFER_STAGING_DIR` | Required absolute non-root staging directory. |
| `ARKNET_TRANSFER_STATE_DIR` | Required absolute non-root state directory distinct from staging. |
| `ARKNET_TRANSFER_WORKER` | Required absolute path to `worker.py` or its executable wrapper. |
| `ARKNET_TRANSFER_LISTEN_INTERFACES` | `0.0.0.0:0` input default; only its port is used. The worker always binds libtorrent to `127.0.0.1`, never an external interface. |
| `ARKNET_TRANSFER_RELAY_EGRESS` | Comma-separated operator relay `host:port` destinations allowed by the container's boot-time firewall; at most 16. Must cover the Agent's verified signed list. Empty means web-seed-only operation. |
| `ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT` | `0`; nonnegative bytes/second, where zero is unlimited. |
| `ARKNET_TRANSFER_MEDIA_DIR` | Optional; read by the worker only. Directory that holds the Agent-finalized `<object_id>.mp4` files. When it is unset, the worker rejects `seed`. |

## Relay-only peer transport

The Agent supplies runtime-only `relays` (`id`, `address`, `spki_sha256`) and
`relay_list_expires_at` on add/refresh and seed requests, exclusively from its
newest unexpired verified Core list. Omitting these fields clears prior relay
authorization: web-seed downloads remain available, but peer connections do not.
They are never persisted in manifests or returned in transfer status.

The worker starts a loopback SOCKS5 connector and tracker bridge. Peer SOCKS
requests accept only the relay virtual range `100.64.0.0/10`; the BitTorrent
handshake selects the full-hash-bound grant. Relays are tried in signed-list
order using TLS 1.3 with the listed SPKI pin. A seeder registers with a relay,
attaches its endpoint attestation to tracker announces, and accepts relayed
streams by dialing libtorrent on `127.0.0.1`. There is no direct-peer fallback.
DHT/LSD/PEX and both uTP directions are disabled. Multiple peers can arrive on
loopback.

libtorrent opens no TCP listener at all once a SOCKS5 proxy is configured, so
the worker runs two sessions. The **download session** is SOCKS5-only and only
dials. The **seed session** has no proxy and listens on `127.0.0.1` at the
configured port (the actual bound port is handed to the connector). It must
never dial a peer, so the tracker bridge strips `peers`/`peers6` from every seed
(`left=0`) announce reply and rejects malformed replies. Seeds carry no web
seeds, and the namespace firewall drops any other attempt.

Incoming/outgoing BitTorrent MSE is disabled because the connector must read the
swarm handshake and the external relay legs already use TLS.

This libtorrent binding also sends web-seed traffic through SOCKS. The only
nonvirtual exception is the exact configured trusted web-seed host/IP and port;
the connector dials the configured origin, not a peer-supplied destination.
Tracker requests go through the loopback bridge to the configured tracker
origin, with no redirect following.

The product container entrypoint installs default-deny IPv4/IPv6 namespace
firewalls before starting the service. Only loopback, established replies, the
control API, and resolved configured tracker/web-seed/relay TCP destinations are
allowed. Compose grants `NET_ADMIN`, `SETUID`, `SETGID`, and `SETPCAP` only for this
initializer, which then uses `setpriv` to run UID/GID 10001 with empty effective,
inheritable, ambient and bounding capability sets. Keep the root-owned
initializer and worker sources immutable. Do not override the entrypoint or
start the service directly in deployment. Changed destination DNS addresses need
a container restart; changing the signed list cannot widen the firewall.

## API configuration notes

Unset/empty optional settings use their defaults; an empty token fails startup.
Keep the listener on loopback. A wildcard address explicitly exposes this
non-TLS mock API; do not expose it to an untrusted network. The service secret
is **not an ArkNet authorization grant**, peer identity, or user credential.

Startup/shutdown records are structured JSON on stdout (`server_started` includes
the actual bound address, service, version, and `mock: true`). SIGINT/SIGTERM stop
accepting connections and drain for up to five seconds; timeout force-closes
connections and exits nonzero. HTTP header/read/write/idle limits are 5/10/10/30
seconds, header limit 8192 bytes, request body limit 4096 bytes. At most 4096
transfers are retained, including terminal entries; a new ID at capacity returns
409 `conflict`, while repeats of existing transfers still work. Restart to reset.

## HTTP contract

Every response is `application/json` with `Cache-Control: no-store`. All `/api`
routes, including unknown routes and simulation routes, require exactly one
HTTP Authorization header carrying the Bearer scheme and the configured service
secret. Comparison uses fixed-length SHA-256
digests and constant-time comparison. Token bytes, request bodies, and credentials
are never logged. Public health is the only unauthenticated endpoint.

| Method and path | Request | Response |
| --- | --- | --- |
| GET `/health` | no body | `{ "healthy": true, "mock": true }` |
| GET `/api/transfer/v1/version` | no body | `{ "version": "0.0.1-dev", "mock": true }` |
| POST `/api/transfer/v1/transfers` | exact add object below | Status, HTTP 200 |
| GET `/api/transfer/v1/transfers` | no body | `{ "transfers": [Status, ...] }`, sorted by object ID; empty array when empty |
| GET `/api/transfer/v1/transfers/{id}` | no body | Status |
| POST `/api/transfer/v1/transfers/{id}/pause` | `{}` | Status |
| POST `/api/transfer/v1/transfers/{id}/resume` | `{}` | Status |
| POST `/api/transfer/v1/transfers/{id}/cancel` | `{}` | Status |
| POST `/api/transfer/v1/seeds` | exact seed object below | `{ "object_id", "state" }` |
| GET `/api/transfer/v1/seeds` | no body | `{ "seeds": [{ "object_id", "state" }, ...] }`, sorted; `[]` when empty |
| DELETE `/api/transfer/v1/seeds/{id}` | no body | `{ "object_id", "state": "REMOVED" }`; 404 if not seeding |

The seed routes exist only on the real libtorrent backend. The mock engine
returns 404 for them. A worker failure returns 503 `unavailable`, and the
response never repeats the request's URLs or tokens. The exact seed object has
these five fields, and they are all required:
`object_id`, `size`, `info_hash_v2`, `tracker_url` (the Core seed grant) and
`torrent_url`. The service validates the URLs against the configured tracker
and Core origins, using the same validators as transfers. It derives the file
name `<object_id>.mp4` itself, so callers can never send a file name or path.
Seed responses carry only the object ID and state, never URLs, hashes or byte
counts.

Exact real-engine add object (all six keys are required):

```json
{"object_id":"media_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1000,"info_hash_v2":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","tracker_url":"https://tracker.example.net/announce/<authorization>","web_seed_url":"https://core.example.net/api/web-seed/v1/media/media_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/<authorization>","torrent_url":"https://core.example.net/api/torrent/v1/media/media_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/<authorization>"}
```

`object_id` is `media_` followed by exactly 64 lowercase hexadecimal characters;
`size` is a nonnegative signed int64 integer (no fraction/exponent syntax);
`info_hash_v2` is exactly 64 lowercase hexadecimal characters. In the real
worker it is verified against private v2 metadata fetched from `torrent_url`.
Both Core URLs must use the configured origin, the same object ID, no userinfo,
query, fragment, or encoded path, and a canonical two-segment raw-base64url token.
The same ID, size, and hash refresh active in-memory URLs; a different size/hash
returns 409 `conflict`. Bearer URLs are never returned in transfer status.

```json
{"object_id":"media_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","state":"DOWNLOADING","total_bytes":1000,"completed_bytes":0,"mock":true,"error_code":""}
```

New adds start `DOWNLOADING` at zero, even when size is zero. Progress uses elapsed
wall-clock time while downloading, not poll count. Reads and operations refresh
this lazy simulation; no background worker or network is needed. At the configured
duration it automatically becomes `COMPLETED` with completed bytes equal to total.
`PAUSED` freezes both bytes and elapsed active time; resume continues remaining
time. Progress arithmetic safely handles int64 maximum sizes without floating
point rounding. All snapshots and mutations are serialized.

- pause: `DOWNLOADING → PAUSED`; repeating pause in `PAUSED` is idempotent.
- resume: `PAUSED → DOWNLOADING`; repeating resume in `DOWNLOADING` is idempotent.
- cancel: `DOWNLOADING | PAUSED → CANCELLED`; repeating cancel is idempotent.
- `COMPLETED`, `CANCELLED`, and `VERIFICATION_FAILED` are terminal; actions cannot
  revive them. Incompatible actions return 409 `invalid_transition`.

## MOCK-ONLY worker report simulation

Exported `engine.Reporter` has:

```go
ReportProgress(id string, completedBytes int64) (engine.Status, error)
ReportCompletion(id string) (engine.Status, error)
ReportVerificationFailure(id string) (engine.Status, error)
```

`*engine.Engine` implements it. Construct with `engine.New(duration, clock)`; a nil
clock means `time.Now`, while deterministic tests supply a clock. These reports
are worker-originated state updates, **not Agent administrative actions**. The
following authenticated **development simulation** endpoints expose that boundary:

| POST path | Exact body |
| --- | --- |
| `/api/transfer/v1/mock/transfers/{id}/progress` | `{ "completed_bytes": 500 }` |
| `/api/transfer/v1/mock/transfers/{id}/complete` | `{}` |
| `/api/transfer/v1/mock/transfers/{id}/verification-failure` | `{}` |

Progress must be monotonic relative to current simulated/reported progress, in
`0..total`, and only while `DOWNLOADING`; it does not itself complete a transfer,
even at total bytes. Normal simulated advancement remains a lower bound after a
manual progress report. Completion/failure may finish `DOWNLOADING` or `PAUSED`:
completion sets bytes to total; verification failure retains bytes and sets
`error_code: "verification_failed"`. Duplicate identical terminal reports are
idempotent; other terminal reports conflict with 409 `invalid_transition`.
This simulator does **not** actually verify data, including when state is
`COMPLETED`. Status polling is report delivery for an Agent consumer; there are
no callbacks, credentials, ArkNet permission grants, or authorization side effects.

Errors contain only `{ "error": "code" }`: 400 `invalid_request`, 401
`unauthorized`, 404 `not_found`, 405 `method_not_allowed`, or 409 `conflict` /
`invalid_transition`. Unknown IDs return 404; malformed IDs return 400. HEAD is
not GET and returns 405. Query strings (including empty `?`), encoded/malformed
paths, oversized bodies, trailing JSON, nulls, unknown/duplicate/case-aliased
fields, and missing required keys are rejected. Titles and filesystem paths are
not accepted. The only accepted remote URL is the optional strictly allowlisted
Core web-seed URL. GET requests must have no body.

## Container

```sh
docker build -t arknet-transfer:mock .
# Explicit wildcard binding inside the container; host publication stays loopback.
docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  -e ARKNET_TRANSFER_TOKEN -e ARKNET_TRANSFER_ADDR=0.0.0.0:8084 \
  -p 127.0.0.1:8084:8084 arknet-transfer:mock
```

Multi-stage Go build, scratch runtime, UID/GID 10001, no shell or media volumes.
The Dockerfile intentionally keeps the process loopback default unless explicitly
overridden as above. The base Dockerfile packages only the Go launcher, not the
Python/libtorrent runtime; complete Node-stack packaging remains separate
deployment work. The
worker implementation and tests cover real byte movement, private-v2 metadata,
resume state, tracker confinement, and authorized Core URL seeds. Relay
orchestration remains outside this endpoint change.

## Worker seed mode

The worker protocol has three seed operations for Peer Serving, exposed
through the `/api/transfer/v1/seeds` routes above. A seed serves a finished,
Agent-finalized object to authorized private-swarm peers:

- `seed` takes exactly `object_id`, `size`, `info_hash_v2`, `tracker_url`,
  `torrent_url` and `file_name`.
  - `file_name` must be exactly `<object_id>.mp4`, a regular file (not a
    symlink) in `ARKNET_TRANSFER_MEDIA_DIR`, with exactly `size` bytes.
  - The worker fetches metadata from the grant's `torrent_url` and checks that
    it is private v2 with the exact hash and size.
  - The torrent's single payload file is mapped to `file_name`.
- Every piece is hash-checked before serving. There is no resume data and no
  `seed_mode`. A file that fails the check becomes `VERIFICATION_FAILED` and is
  removed from the session.
- A seed is upload-only (`upload_mode`), so it can never download or write to
  the media directory. It has no web seeds. DHT, PEX and LSD stay disabled, and
  tracker and web-seed overrides apply.
- Repeating `seed` for the same object, size and hash only replaces the
  tracker URL and re-announces. A different size or hash, or an active download
  of the same object, returns `conflict`.
- `unseed {object_id}` removes the torrent and never deletes the file.
  `seeds` lists `{object_id, state, total_bytes, uploaded_bytes}`, where
  `state` is `CHECKING`, `SEEDING` or `VERIFICATION_FAILED`.
- Seeds are kept in memory only. They are never written to the state
  directory, so a restart serves nothing until the Agent sends `seed` again
  with a fresh Core seed grant.

## Verify

```sh
# Worker tests need python3-libtorrent, so run them in the transfer image:
docker run --rm --network none --user 10001:10001 -v "$PWD:/src:ro" -w /tmp \
  --entrypoint python3 arknet-transfer:0.0.1-dev -B -m unittest discover -s /src -p 'worker*_test.py'
go test ./...
go test -race ./...
go vet ./...
go list -m all                    # only arknet/transfer
gofmt -l *.go engine/*.go
```

Tests cover auth, health/version, add/list/status, elapsed progress, pause/resume/
cancel, terminal reports, zero-size, int64 overflow boundaries, identity conflicts,
strict request validation, capacity, concurrent reports/actions/reads, listener
failure, shutdown drain/deadline, and a compiled-process real-listener smoke test
that exercises every endpoint family, sends SIGTERM, requires exit 0 and
`shutdown_complete`, and checks invalid configuration fails without token leaks.
Development used successive failing-test → passing-implementation tracer slices.
