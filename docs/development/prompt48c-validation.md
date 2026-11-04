# Prompt 48C validation — tracker relay-only advertisements

Validated 2026-11-03 on branch `phase20/48c-tracker-relay-endpoints`.
The working tree also contains the previously tested, uncommitted Prompt 48B
changes. No commit or merge was made for either prompt during 48C.

## Acceptance evidence

- `tracker/internal/tracker/relay_endpoints_test.go` checks exact compact
  responses for IPv4 and IPv6 origins (and an opaque proxy origin). Responses
  contain only the signed `100.64.0.0/10` virtual endpoint, not an origin IP or
  the announced port. State-schema assertions prohibit origin-address fields
  in peer records and parsed announces. The handler never reads `RemoteAddr`.
- Complete announces without an endpoint token are refused. Tokenless
  downloaders succeed and are not advertised. A valid endpoint token does not
  make an incomplete downloader publishable.
- Forged, expired, other-subject, and other-full-hash tokens are rejected before
  swarm mutation, including on stop requests. The wrong-hash test preserves
  the first 20 bytes, exercising the full v2 hash boundary.
- The verifier's tests re-sign malformed claims with the trusted private key
  to exercise semantic guards beyond signature rejection. They cover version,
  expiry equality, subject, hash, virtual range, port, payload length, signer,
  canonical encoding, unknown keys and multiple configured relay keys.
- Endpoint registration and control-channel renewal return signed tokens
  expiring at exactly the presented seed grant expiry. Dedicated key config
  rejects reuse of either Core verification key and inconsistent private keys.
- Refresh tests prove a re-announced renewed endpoint extends presence, but
  never beyond the Core grant expiry. Endpoint expiry also prunes before the
  Core grant or peer TTL when it is earlier.
- Existing tracker behavior tests now publish authenticated relay endpoints;
  ownership, stop authorization, capacity, numwant and downloader privacy
  checks continue to pass.

## Execution

Commands passed:

```sh
(cd tracker && go vet ./... && go test -race -count=2 -timeout=120s ./...)
(cd relay && go vet ./... && go test -race -count=2 -timeout=120s ./...)
bash scripts/check-all.sh --logs ~/.hermes/cache/scratch/48c/gate
```

The repository gate returned **13 passed, 0 failed**, `EXIT 0`, including Core,
Agent, tracker, relay, UI, harness, deploy, PostgreSQL and worker checks.
The combined Core/tracker/relay Compose overlays render successfully with
placeholder paths and explicit environment configuration (`config --quiet`).

`tracker/relay_integration_test.go` builds and launches **real relay and tracker
binaries**, creates separate grant, revocation, endpoint and TLS keys, registers
a seed over certificate-verified TLS 1.3, and announces the actual returned
token. The tracker receives only public verification keys. It advertises the
exact signed endpoint and refuses token reuse by another subject and a
tokenless seed. Both processes are shut down by the test. Shared hard-coded
golden vectors bind the relay signer and tracker verifier formats.

## Mutation checks

All **10 targeted tracker guard mutations** caused behavioral test failures:
relay signature, version, subject, full hash, expiry, virtual prefix, minimum
port, payload canonical encoding, complete-peer token requirement, and the
Core-grant advertisement expiry bound. Production sources were restored after
each mutation. Evidence: `~/.hermes/cache/scratch/48c-mutations.json`.
Relay signer/config/error paths were also mutation-checked during implementation.

## Independent review

The tracker security/specification review found no blocking security or logic
defects. It independently reran tracker internal race tests, vet and the tracker
process test. Its non-blocking suggestions were additional direct regression
cases for wrong-domain/canonical signature encodings and non-stopped invalid
announces. Real relay/tracker interoperability and complete module tests were
verified separately by the parent session as recorded above.

## Deliberate rollout boundary

The full legacy product-swarm/MVP E2E gate was **not rerun for 48C**. The current
Node transfer client does not yet inject endpoint tokens; its seed announces
now fail closed rather than advertising origins. Prompt 48D supplies that
connector and restores end-to-end peer-transfer coverage with relay routing.
No existing E2E assertions were weakened or replaced with direct-peer fallback.
ARKSEC-004 remains open pending that integration and privacy/egress proof.

Endpoint tokens attest registration, not ongoing reachability. A disconnected
or revoked registration can remain advertised until token/grant expiry or peer
TTL; the relay refuses the underlying connection. Tokens are bearer attestations,
not proof of client private-key possession.
