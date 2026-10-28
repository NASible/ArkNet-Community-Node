# Swarm authorization tokens

ArkNet Core issues short-lived, signed authorization for one Node and one
BitTorrent v2 swarm. These credentials are bearer tokens and must be delivered
over verified TLS, kept out of logs, and supplied only to the local transfer
service and the dedicated ArkNet tracker.

## Issuance

An approved and trusted Node sends its existing Ed25519-signed federation
request to:

```text
POST /api/federation/v1/nodes/{node_id}/media/{media_id}/tracker-grants
```

For backward compatibility, an empty signed body requests `download` only.
Seeding is always explicit. A Node may instead sign one strict JSON object:

```json
{"actions":["download"]}
{"actions":["seed"]}
{"actions":["download","seed"]}
```

The body may contain no other fields, duplicate actions, unknown actions,
trailing values, or an empty action list. The exact body bytes are covered by
the Node request signature. Core authenticates the route's Node ID, then checks
that the Node remains approved and trusted and that the requested media object
is active with a canonical full v2 info hash. A rejected, blocked, pending, or
otherwise revoked Node receives no new authorization.

A successful no-store response contains the tracker URL, expiry, and canonical
granted action list. When Core's HTTP web seed is enabled and the authorization
includes `download`, the response also contains a `web_seed_url` using the same
short-lived signed token. A seed-only response omits that field, and its token is
not valid for the endpoint. The response contains no global-user ID, profile,
watch history, or local username.

## Signed payload

The token is:

```text
base64url-no-padding(payload) + "." + base64url-no-padding(ed25519_signature)
```

The canonical version 2 payload is exactly 58 bytes:

| Offset | Size | Meaning |
|---:|---:|---|
| 0 | 1 | version, exactly `2` |
| 1 | 8 | expiry as unsigned big-endian Unix seconds |
| 9 | 16 | opaque pairwise Node/swarm subject |
| 25 | 32 | full BitTorrent v2 SHA-256 info hash |
| 57 | 1 | action mask |

Action bits are:

```text
0x01 download
0x02 seed
```

The mask must be non-zero and may contain no unknown bits. Core signs the entire
payload with its dedicated tracker signing key. The tracker receives only the
public verification key and therefore needs no Core database access.

The subject is stable for one authenticated Node and one full v2 hash but differs
across Nodes and swarms. It is derived from a domain-separated Ed25519 signature
made with Core's dedicated tracker private key, then hashed and truncated to 16
bytes. It reveals no Node ID and cannot be dictionary-tested with the public key.

## Tracker enforcement

The tracker validates canonical encoding, signature, version, expiration, action
mask, and the full swarm scope before touching swarm state:

- `info_hash` must match the first 20 bytes of the signed full v2 hash.
- An announce with `left > 0` requires `download`.
- An announce with `left == 0` requires `seed`.
- A stopped announce is checked against its declared role in the same way.
- The pairwise subject may update or stop only its own peer record.

A wrong swarm, expired token, missing action, unknown action bit, or invalid
signature is rejected. Knowing a Node ID, tracker endpoint, and info hash is not
enough to mint or alter authorization.

These action checks authorize tracker participation. They do not make an
untrusted BitTorrent client honest: `arknet-transfer` must also respect the same
actions when deciding whether to request pieces or upload bytes.

## Privacy and logging

The token contains no global-user identity. The tracker stores only the minimum
in-memory Node/swarm subject and peer presence described in
[private-tracker.md](private-tracker.md). Core and tracker application logs must
not include the token, complete announce URL, query string, subject, peer ID, or
client address.
