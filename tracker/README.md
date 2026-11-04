# ArkNet v2 private HTTP tracker

A standalone, in-memory BitTorrent HTTP tracker for private ArkNet BEP 52/v2 swarms. It exposes only `GET /health` and `GET /announce/{grant}`. It has no scrape, public announce, DHT, PEX, database, user identity, or node identity API.

## Verification key and grant format

`ARKNET_TRACKER_VERIFY_KEY_FILE` is required. The file must contain exactly one **canonical, standard padded base64** encoding of 32 Ed25519 public-key bytes, with no surrounding whitespace or trailing newline. Raw binary, URL-safe base64, unpadded base64, private keys, and extra content are rejected. Mount this file read-only.

A grant is independently verifiable and contains no direct identity:

```text
base64.RawURLEncoding(payload) + "." + base64.RawURLEncoding(ed25519_signature)
```

The signed canonical payload is exactly 58 bytes:

| Offset | Size | Meaning |
|---:|---:|---|
| 0 | 1 | version byte, exactly `2` |
| 1 | 8 | expiry Unix seconds, unsigned big-endian |
| 9 | 16 | opaque pairwise Node/swarm subject |
| 25 | 32 | full v2 info hash |
| 57 | 1 | action mask |

The action bits are `download=0x01` and `seed=0x02`. A mask may authorize either action or both; zero and masks containing any unknown bit are invalid. The pairwise subject is stable only for one authenticated Node and one full v2 hash. It lets a refreshed grant update that Node's own record without exposing the Node ID or linking the Node across swarms. The grant is expired when current Unix time is greater than or equal to its expiry. The first 20 bytes of the signed 32-byte v2 hash must exactly equal the binary `info_hash` announce value (the BEP 52 tracker key).

See [relay endpoint token format and rollout](../docs/relay-endpoint-tokens.md)
for signing bytes, key distribution, renewal and the bearer-token boundary.

## Configuration

| Variable | Default | Rules |
|---|---|---|
| `ARKNET_TRACKER_VERIFY_KEY_FILE` | none | Required base64 public-key file described above |
| `ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE` | unset | Optional file with 1–16 distinct relay endpoint-signing public keys, one canonical padded-base64 32-byte key per line. Must differ from the Core grant key. Without keys, all seed announces are refused; downloaders still work. |
| `ARKNET_TRACKER_BIND_ADDR` | `127.0.0.1:8080` | Valid `host:port`; use `:8080` only for intentional network exposure |
| `ARKNET_TRACKER_ANNOUNCE_INTERVAL` | `30m` | Positive Go duration |
| `ARKNET_TRACKER_PEER_TTL` | `45m` | Positive Go duration |
| `ARKNET_TRACKER_SHUTDOWN_TIMEOUT` | `10s` | Positive Go duration |

The loopback default is deliberate. Deploy behind only the network controls intended for the private tracker.

## HTTP contract

`GET /health` returns `200` and `ok`.

`GET /announce/{grant}` requires exactly one each of binary 20-byte `info_hash`, binary 20-byte `peer_id`, `port` (1–65535), unsigned decimal `uploaded`, `downloaded`, and `left`, and `compact=1`. Optional fields are `event` (`started`, `completed`, or `stopped`) and unsigned decimal `numwant` (capped at 50).

For compatibility with libtorrent 2.0.x private v2 announces, these optional fields are also accepted exactly once:

- `corrupt` and `redundant`: unsigned decimal
- `key`: 1–64 visible ASCII bytes (libtorrent normally sends 8 hexadecimal characters)
- `no_peer_id` and `supportcrypto`: exactly `0` or `1`

Unknown and duplicate parameters are rejected. An announce with request `left>0` requires the signed download action; `left=0` requires the signed seed action. This rule also applies to `event=stopped`, based on the request's `left` value, and action denial returns `403` before any swarm mutation. Complete peers also require `endpoint_token`, signed by a configured relay and bound to the same pairwise subject and full v2 hash. Invalid or expired tokens return `403` before state mutation, including stops; duplicate tokens return `400`. Downloaders may omit the token and are never advertised. The tracker never reads or stores `RemoteAddr`, forwarded addresses, or the announced port. Only the signed virtual endpoint is stored for a complete peer. Responses are bencoded dictionaries containing `interval`, `complete`, `incomplete`, compact IPv4 `peers`, and an empty `peers6` (virtual endpoints are IPv4). The requester is excluded. `event=stopped` removes only the record owned by the signed pairwise subject. Each subject owns at most one live record and cannot overwrite or stop another subject's peer ID. Stale peers are pruned before admission; each signed full-hash swarm is capped at 1024 live records. Request targets are capped at 8192 bytes and server headers are bounded.

The service deliberately emits no request logs. JSON logs are limited to startup, shutdown, and configuration/listener/server errors; grants, hashes, query strings, peer IDs, and client IPs are not logged.

## Run and verify

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o arknet-tracker .
ARKNET_TRACKER_VERIFY_KEY_FILE=/run/secrets/tracker-verify-key ./arknet-tracker
```

SIGINT and SIGTERM stop acceptance and drain active requests up to the configured shutdown timeout. A timeout forces connections closed and exits nonzero.

## Container

Build from this directory:

```sh
docker build -t arknet-tracker .
docker run --rm --read-only --cap-drop=ALL \
  -p 127.0.0.1:6969:6969 \
  -e ARKNET_TRACKER_VERIFY_KEY_FILE=/run/secrets/tracker-verify-key \
  -v "$PWD/tracker-verify-key:/run/secrets/tracker-verify-key:ro" \
  arknet-tracker
```

The pinned Alpine runtime sets `ARKNET_TRACKER_BIND_ADDR=0.0.0.0:6969`, exposes port 6969, runs as UID/GID 65534, and includes a BusyBox `wget` healthcheck against `/health`. It requires no writable filesystem.
