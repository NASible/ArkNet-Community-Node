# ArkNet private relay

A standalone, in-memory TCP relay that lets ArkNet Nodes exchange BitTorrent
v2 payload without learning each other's addresses (ARKSEC-004; design:
[private relay design](../docs/private-relay-design.md), Prompt 48A). It has
no database, no persistence, and no Node or user identity: it authorizes with
the same Core-signed, swarm-scoped grants as the private tracker.

Not yet used by Nodes or the tracker: Core's relay list and revocation push
are Prompt 48B, tracker endpoint advertisement is 48C, and the Node connector
is 48D.

## How it works

- A **seeder** connects with a **seed** grant and gets a random virtual
  endpoint in `100.64.0.0/10`. That connection stays open as its control
  channel.
- A **downloader** connects to an endpoint with a **download** grant for the
  same full v2 info hash. The relay notifies the seeder (`incoming`, with
  only a session ID), the seeder opens a second connection to `accept`, and
  the relay splices the two streams. Neither side is told the other's
  address.
- A session lasts while **both** sides hold an unexpired grant for its swarm.
  Presenting a later grant (`renew`, on the seeder's control channel or as a
  one-line request) extends it. When a grant expires, its sessions and
  registrations are closed within one sweep (1 s).
- A **revocation** signed by Core closes the listed pairwise subjects'
  registrations and sessions at once. Grants those subjects held before the
  revocation are refused until they could have expired (grant lifetime + 60 s
  skew), so a revoked Node cannot reconnect with a grant it already has.

## Wire protocol

After TLS 1.3, the client sends one JSON line (at most 4096 bytes, within 10 s)
and the relay answers with JSON lines:

| Request | Grant | Success |
| --- | --- | --- |
| `{"op":"listen","grant":G}` | seed | `{"endpoint":"100.x.y.z:p","expires_at":N}`, then the control channel: relay sends `{"op":"incoming","session":ID}`; the seeder may send `{"op":"renew","grant":G}` → `{"op":"renewed","expires_at":N}` |
| `{"op":"connect","endpoint":E,"grant":G}` | download, same info hash | `{"ok":true}`, then raw bytes |
| `{"op":"accept","session":ID,"grant":G}` | the registered seeder's seed grant | `{"ok":true}`, then raw bytes |
| `{"op":"renew","grant":G}` | either | `{"op":"renewed","expires_at":N}` |

Failures answer `{"error":"unauthorized"}` (grant invalid, expired, wrong
action, too long, or revoked), `{"error":"no_route"}` (no such endpoint, or
another info hash), or `{"error":"unavailable"}` (seeder did not accept within
10 s, or too many pending sessions), then close. Unknown operations, oversized
or late headers close without an answer. A session ID is single use.

## Revocations

`POST /api/relay/v1/revocations` on the control listener, body: the token.

```text
token   = base64url(payload) "." base64url(Ed25519(key, "ARKNET-RELAY-REVOCATION-V1\x00" || payload))
payload = version(1)=1 | issued_at(u64 BE, Unix s) | id[16] | SHA-256(relay ID)[32] | count(u16 BE, 1..4096) | subjects[16*count]
```

The relay accepts it only if the signature verifies with the **revocation
key** (which must differ from the grant key), the audience is this relay's
ID, `issued_at` is within 60 s of the relay's clock, and the ID has not been
used (`204`). Otherwise `401` (invalid, other relay, stale), `409` (replay),
`413` (too large). `relay/internal/revocation` is the reference
implementation; Core signs these in Prompt 48B.

## Configuration

| Variable | Default | Rules |
| --- | --- | --- |
| `ARKNET_RELAY_GRANT_VERIFY_KEY_FILE` | required | Core's tracker-grant public key (the tracker's verification key file). Canonical padded base64 of 32 bytes |
| `ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE` | required | Core's relay-revocation public key, same format; must differ from the grant key |
| `ARKNET_RELAY_ID` | required | 1–64 of `a-z 0-9 . -`; the revocation audience |
| `ARKNET_RELAY_TLS_CERT_FILE`, `ARKNET_RELAY_TLS_KEY_FILE` | required | PEM certificate chain and key; TLS 1.3 only |
| `ARKNET_RELAY_BIND_ADDR` | `127.0.0.1:7443` | Relay (TLS) listener |
| `ARKNET_RELAY_CONTROL_BIND_ADDR` | `127.0.0.1:7080` | Health and revocations (plain HTTP; the signature authenticates). Keep it off public interfaces |
| `ARKNET_RELAY_MAX_CONNECTIONS` | `4096` | 1–65536; further connections are closed at once |
| `ARKNET_RELAY_MAX_GRANT_LIFETIME` | `5m` | Core's grant lifetime, 1m–1h. Longer grants are refused; it also sets how long revoked subjects stay refused |
| `ARKNET_RELAY_SHUTDOWN_TIMEOUT` | `10s` | 1s–5m |

At most 64 sessions may wait for one seeder to accept.

## Logging

JSON logs carry event names, reasons and counts only: never addresses, grants,
info hashes, subjects, endpoints or session IDs (tested in
`internal/relay` and by the process test). The startup line reports the
relay's own listen addresses.

## Run and verify

```sh
go test -race ./...
go vet ./...
go build -o arknet-relay .
```

Container: `docker build -t arknet-relay .` (Alpine runtime, UID/GID 65534,
read-only filesystem works, health check on the control listener). With Core,
use the `deploy/core/compose.relay.yaml` overlay (see
[deploy/core/README.md](../deploy/core/README.md)).
