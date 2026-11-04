# Private ArkNet tracker

ArkNet uses a dedicated, authorization-gated HTTP tracker for private BitTorrent
v2 swarms. The tracker is a separate process from the Core API and does not have
a Core database connection.

## Trust boundary

1. A Node authenticates to Core with its existing Ed25519 node proof.
2. For an active catalog object with a 64-character lowercase v2 info hash, the
   Node requests:

   ```http
   POST /api/federation/v1/nodes/{node_id}/media/{media_id}/tracker-grants
   ```

   An empty signed body requests download-only authorization. A Node opts into
   seeding by signing a strict JSON body such as
   `{"actions":["download","seed"]}`; no query parameters are accepted.
3. Core returns a short-lived tracker URL, expiry, and canonical action list. The
   URL contains an opaque bearer grant and must not be logged, placed in torrent
   metadata, sent to the browser, or shared with another Node.
4. The Node supplies that URL only to its local transfer service. The transfer
   service and tracker receive no Core administrative or enrollment credentials.
5. The tracker verifies the grant using Core's dedicated tracker public key.
   It has no access to user profiles, watch history, local usernames, or Core's
   signing key.

Knowing the tracker origin and an info hash is not authorization. An announce
without a valid, unexpired Core-signed grant is rejected.

## Grant format

A grant is:

```text
base64url(payload) + "." + base64url(ed25519_signature(payload))
```

Both base64url components are raw/unpadded. The canonical payload is exactly:

| Offset | Size | Meaning |
|---:|---:|---|
| 0 | 1 | version (`2`) |
| 1 | 8 | expiry as unsigned big-endian Unix seconds |
| 9 | 16 | opaque pairwise Node/swarm subject |
| 25 | 32 | full BitTorrent v2 SHA-256 info hash |
| 57 | 1 | action mask (`0x01` download, `0x02` seed) |

The payload intentionally contains no Node ID, global user ID, local username,
profile identifier, title, watch state, or filesystem path. Core derives the
subject as the first 16 bytes of SHA-256 over a domain-separated Ed25519
signature made with Core's dedicated tracker signing key over the authenticated
Node ID and full v2 hash. It is stable only within that Node/swarm pair, differs
across swarms, and cannot be reversed or dictionary-tested by the tracker using
only its public verification key.
This lets refreshed grants update exactly one owned peer record without letting
one authorized Node overwrite, stop, or consume unbounded slots for other
authorized Nodes. A grant is scoped to one full v2 hash, a nonempty subset of
the download/seed actions, and expires automatically. Unknown action bits are
invalid.

The bearer grant authorizes its holder. Protect the complete announce URL like
a credential. Core and tracker application logs never record it.

## BitTorrent v2 announce behavior

The only swarm endpoint is:

```http
GET /announce/{grant}?info_hash=...&peer_id=...&port=...&uploaded=...&downloaded=...&left=...&compact=1
```

BEP 52 announces represent a v2 torrent to an HTTP tracker using the first 20
bytes of the full 32-byte v2 hash. The tracker verifies that this tracker key
matches the full hash signed into the grant. Internally, swarms remain keyed by
the full 32-byte value, so two full hashes with the same 20-byte prefix are not
combined.

An announce with `left > 0` requires the signed `download` action. An announce
with `left == 0` requires the signed `seed` action. Action checks occur before
any peer-state mutation, including for `stopped` announces.

The service accepts the standard optional fields emitted by libtorrent 2.x,
validates duplicates and numeric values strictly, caps `numwant`, bounds each
swarm's peer records, expires stale peers, and returns compact IPv4 `peers` and
an empty `peers6` value. It never reads or stores socket source addresses,
forwarded addresses or the announced port. Complete peers must include a
relay-signed `endpoint_token`, bound to their grant subject and full v2 hash.
The tracker verifies it with configured relay public keys. Forged, expired,
other-subject and other-hash tokens are rejected before state mutation. An
announce without a token is accepted only for downloaders, never advertised.

Only complete peers are advertised. These are peers announcing `left == 0` with
a signed `seed` action and a valid relay endpoint token. A Node that is still downloading is counted in
`incomplete`, but its address is never returned to anyone. So receiving never
lets another peer request a Node's partial pieces. Downloaders connect to
seeds, seeds never need downloader addresses, and Peer Serving stays opt-in.

There is no scrape endpoint, public announce endpoint, public torrent index,
DHT, PEX, local peer discovery, user directory, or arbitrary tracker proxy.
Tracker state is deliberately in memory; restarting it discards peer presence
and clients repopulate swarms on their next authorized announce. For each live
record it keeps only the full v2 hash, 16-byte opaque pairwise subject, peer ID,
verified virtual IPv4 endpoint (complete peers only), complete/incomplete bit,
last-seen time and the earlier grant/endpoint-token expiry. No origin address
is retained, including for downloaders.
Those fields are needed to isolate the swarm, enforce record ownership, return
peers, count completion, and expire stale records; none are written to logs or
durable storage.

See [relay endpoint tokens](relay-endpoint-tokens.md) for the relay-signed
registration token format and deployment boundary. Node announce injection
follows in Prompt 48D; legacy tokenless seeds now fail closed.

## Configuration

Core grant issuance is disabled when both variables are absent:

```dotenv
# Origin only; no path, query, fragment, or userinfo.
ARKNET_TRACKER_ANNOUNCE_URL=https://tracker.example
ARKNET_TRACKER_SIGNING_KEY_FILE=/run/secrets/arknet-tracker-signing-key
```

The configured URL must be an origin with no userinfo, path, query, or fragment.
Remote deployments require HTTPS. Plain HTTP is accepted only for loopback or
the exact `arknet-tracker` Compose service name. Configuring only one variable
is an error.

Tracker configuration:

```dotenv
ARKNET_TRACKER_BIND_ADDR=0.0.0.0:6969
ARKNET_TRACKER_VERIFY_KEY_FILE=/run/secrets/arknet-tracker-verify-key
ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE=/run/config/relay-endpoint-public-keys
ARKNET_TRACKER_ANNOUNCE_INTERVAL=30m
ARKNET_TRACKER_PEER_TTL=45m
ARKNET_TRACKER_SHUTDOWN_TIMEOUT=10s
```

The private-key file contains exactly the canonical padded-base64 encoding of a
64-byte Ed25519 private key, with no trailing newline. The verification-key file
contains canonical padded-base64 for its 32-byte public key. Mount the private
file only into Core and the public file only into the tracker.

The tracker serves plain HTTP itself. Production deployments must terminate TLS
without writing request paths or query strings to access logs. Origin addresses are irrelevant to
peer selection; a proxy does not need to preserve or forward them.

## Minimal metadata and logs

Runtime peer state contains only:

- full v2 swarm hash (in memory)
- 20-byte peer ID
- relay-verified virtual IPv4 endpoint (complete peers only; no origin IP or port)
- completion flag
- last-seen time

These values are needed to coordinate the swarm and are never written to the
application log. Logs are limited to configuration errors, listener lifecycle,
and graceful shutdown. The service has no persistent database.

## Key rotation

Generate an independent Ed25519 keypair for tracker grants; do not reuse Node,
Core administration, TLS, or enrollment keys. Rotate by deploying the new public
key to the tracker and matching private key to Core together, then restarting
both services. Existing grants cease to work and Nodes must request new ones.
