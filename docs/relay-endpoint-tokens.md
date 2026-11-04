# Relay endpoint tokens (Prompt 48C)

A relay attests that a seed-authorized pairwise subject registered a virtual
endpoint for one full v2 info hash. The tracker advertises **only** these
verified endpoints, not the seeder's announce origin. Downloaders never need
an endpoint token and are never advertised.

## Issuance and announce

1. A seeder registers with its Core-issued seed grant over TLS 1.3.
2. The relay returns `endpoint`, `expires_at`, and `endpoint_token`. On a
   successful seeder control-channel renewal it returns a fresh token for the
   same endpoint and the renewed grant's expiry.
3. The seeder includes `endpoint_token=TOKEN` in its tracker announce alongside
   its Core grant. The tracker verifies the relay signature and binds the
   subject and full hash to the Core grant before modifying any swarm state.
4. Complete peers (`left=0`) require a seed grant **and** this endpoint token.
   Tokenless announces are accepted only for downloaders (`left>0`, download
   grant). A supplied invalid token is rejected even on a downloader or stop
   announce. Duplicate parameters and oversized tokens are rejected.

The tracker stores the signed compact virtual endpoint for complete peers;
for downloaders it stores no address. It never reads `RemoteAddr` or forwarded
address headers to parse or store an announce. The announced port is checked
for protocol validity but not retained or used as the advertised port. Responses
contain virtual IPv4 endpoints in `peers` and an empty `peers6`, even when the
announce itself arrives over IPv6.

## Signed bytes

```text
token = raw-base64url(payload) + "." + raw-base64url(signature)
signature = Ed25519(endpoint_key, "ARKNET-RELAY-ENDPOINT-V1\x00" || payload)
```

Both encodings are canonical, unpadded URL-safe base64. The payload is exactly
63 bytes:

| Offset | Bytes | Meaning |
| ---: | ---: | --- |
| 0 | 1 | Version, exactly 1 |
| 1 | 8 | Expiry, unsigned big-endian Unix seconds |
| 9 | 16 | Pairwise subject from the Core seed grant |
| 25 | 32 | Full v2 info hash from that grant (not just its 20-byte tracker prefix) |
| 57 | 4 | Virtual IPv4 address in `100.64.0.0/10` |
| 61 | 2 | Port, unsigned big-endian, 1024–65535 |

A token expires at the exact boundary `now >= expiry`. An advertisement expires
at the **earlier** of the announcing Core grant or the endpoint token (and may
be pruned earlier by the tracker peer TTL). A renewed token must be announced
again before it extends tracker presence.

## Key distribution

- Relay: `ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE` holds its dedicated
  canonical padded-base64 64-byte Ed25519 private key. It must differ from
  the keys used to verify Core grants and Core revocations.
- Tracker: `ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE` holds only endpoint-signing
  **public** keys, one canonical padded-base64 32-byte key per line, at most
  16 distinct keys. The Core grant verification key is not accepted as a relay
  endpoint key. Configure the operator-approved relays only. Removing a key
  requires a tracker restart, which also drops all in-memory advertisements.
- No configured tracker relay keys means no complete-peer publication;
  downloaders remain accepted. There is no compatibility fallback to origins.

The endpoint token is a bearer attestation, not proof of possession or proof
that a control connection is still live. After disconnection or revocation,
its advertisement may remain until token/grant expiry or peer TTL; the relay
itself refuses the connection. A malicious signer is trusted to attest
registrations, but cannot make the tracker advertise arbitrary origin
addresses: the signed endpoint must still be in the virtual range.

Logs must never contain these tokens, grants, subjects, hashes or endpoints.
The tracker has no request logging; reverse proxies must suppress credential-
bearing paths and query strings too.

## Rollout boundary

Prompt 48C changes tracker publication to fail closed. The Node connector and
its injection of endpoint tokens into libtorrent announces are Prompt 48D.
Until 48D is deployed, legacy seed announces receive `403` rather than
publishing origin addresses. No direct-peer fallback is provided.
