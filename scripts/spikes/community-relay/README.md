# Prompt48E community relay — THROWAWAY protocol prototype

## Verdict: INVALIDATED for the requested strong privacy / egress-saving goals

**DO NOT SHIP.** The transport/authentication experiment passes; that does not validate a deployment. One community leg hides the other Node's origin from a passive, non-colluding community observer. A community colluding with the opposite Node (including operating that Node itself) knows both origins. The same community on both legs sees both directly. The operator still emits the full payload; a community hop saves **zero operator application egress**, and adds TLS wire overhead.

This is an independent Go-standard-library module. No product APIs, product responses, enrollment workflow, transfer implementation, or production grant format are reused. Core is an in-process mock with **distinct ephemeral Ed25519 admission and grant keys** and distinct signed claim kinds. All identities and keys are generated fresh, kept in memory, and not written to results.

## Reproduce

From any directory:

```sh
python3 scripts/spikes/community-relay/run.py --mib 32 --reps 3
```

The tests give each party its own loopback address (`127.0.0.2`–`.5`), so run
them on Linux. macOS has only `127.0.0.1`, and `go test` there fails with
`can't assign requested address`. To run only the tests:

```sh
docker run --rm --network none -v "$PWD/scripts/spikes/community-relay:/src:ro" -w /src \
  -e GOCACHE=/tmp/c golang:1.26-alpine go test -count=1 ./...
```

The runner builds a UUID-named image using `golang:1.26-alpine`, runs `go test -v ./...` and `go vet ./...` in the build stage, and copies the static executable into `scratch`. It creates the runtime container with:

```sh
docker create --name UNIQUE_NAME --network none --cap-drop ALL --read-only \
  --security-opt no-new-privileges:true UNIQUE_IMAGE -mib 32 -reps 3
docker start -a UNIQUE_NAME
```

`commands.json` contains the exact commands/UUIDs for the saved run. The runner inspects and records the actual Docker settings before running. There are no published ports or mounts; the binary runs as `65532:65532` on Linux arm64. It removes only its own container/image in `finally`, then verifies that both names are absent. Build cache/base images are not pruned. No product files are touched and no commits are made.

## Isolation and protocol scope

NodeA `127.0.0.2`, NodeB `127.0.0.3`, CommunityC `127.0.0.4`, and OperatorO `127.0.0.5` are **logical roles in one process and one Linux network namespace**, not separate containers, processes, memory boundaries, or namespaces. Socket observations are real TCP `LocalAddr`/`RemoteAddr` values (Go's getsockname/getpeername-backed addresses), not assigned labels masquerading as measurements. A malicious process with memory access could read everything; the privacy experiment concerns the explicitly instrumented C transport observer only.

Compared routes, carrying A's payload to B:

- `A-O-B`: A inner TLS to O; B inner TLS to O.
- `A-C-O-B`: A inner TLS to O over C's authenticated outer mTLS tunnel; B direct inner TLS to O.
- `A-O-C-B`: A direct inner TLS to O; B inner TLS to O over C's authenticated outer mTLS tunnel.
- `A-C-O-C-B`: intentionally **unsafe negative fixture**, both inner connections through the same C; not a compliant route.

Nodes authenticate O's CA/name and pin O's SPKI **before sending any grant**. O authenticates the Node certificate inside the inner TLS session. For community connections O first terminates outer mTLS, matches C's actual client-certificate SPKI to a Core-signed admission (`community-C`, O audience, C SPKI, expiry), then runs `tls.Server` over the decrypted outer stream. Signed inner grants bind Node identity, send/receive role, full payload hash, operator audience, expiry, and `direct` versus `community-C` route. The actual validated outer identity determines the route O accepts; a client-supplied route cannot replace that identity.

C has a fixed O target and no target-selection API. The open-proxy negative sends a real CONNECT request for a separate loopback target and checks rejection plus no connection to that target. The stronger direct-listener bypass negative uses an actual malicious C TCP reproxy, sourced from `.4`, carrying A's intact inner TLS to O-direct after C revocation. O refuses its still-C-bound grant.

Admission is checked after mTLS before inner TLS, and every 10 ms for active tunnels. Revocation is an in-process mock state transition, **not a production signed revocation distribution protocol**. Tests verify EOF/close (not a timeout) within one second for revocation and naturally expiring admission, and deny subsequent connections. The expiry fixture issues the short-lived admission before opening its active tunnel. Peer grants are fresh and checked at authorization; this spike does not implement grant refresh, replay storage, or grant-driven active-session expiry.

## Saved measured results

`result.json` is sanitized structured evidence: **20/20 protocol/negative-control checks PASS**, nine measured 32 MiB transfers (three per compliant topology), and one 1 MiB unsafe fixture. Every payload is received and SHA-256 verified. `result.json` is the committed evidence. `run.py` also writes `build.log` (eight passing unit/integration tests and a clean vet) and `checks.log` (runtime PASS/FAIL checks) next to it. Like all `*.log` files they are git-ignored, so rerun to regenerate them.

Medians from the saved run (32 MiB = 33,554,432 application payload bytes):

| Route | Throughput MiB/s | O application egress bytes | O TLS wire egress bytes | C TLS wire egress bytes |
|---|---:|---:|---:|---:|
| A-O-B | 456.88 | 33,554,432 | 33,603,428 | 0 |
| A-C-O-B | 361.16 | 33,554,432 | 33,605,376 | 33,651,281 |
| A-O-C-B | 337.13 | 33,554,432 | 33,695,466 | 33,606,030 |

O wire overhead versus baseline: +1,948 bytes for the sender-side community leg; +92,038 bytes for the receiver-side community leg. The latter sends the full payload through **both** inner and outer TLS on O's egress. Receiver-side median O direct wire egress is 1,904 bytes and O outer wire egress is 33,693,562 bytes: their sum is the reported total, not an apparent saving obtained by counting only O-direct.

### What is counted

- O application egress: successful forwarded payload bytes written into B's inner TLS, not authorization/control bytes.
- O wire egress: successful writes on **every raw operator TCP connection outside all TLS layers**, including O-to-Node and O-to-C, handshake and control. The total is asserted equal to direct + outer counters. Nested TLS bytes are not double-counted.
- C wire egress: successful raw writes to O plus raw writes to its adjacent Node. This includes both C directions' outbound connections.
- These are TLS-stream bytes, **not** Ethernet/IP/TCP packet sizes, ACKs, retransmissions, or billed cloud-network bytes.
- Throughput starts immediately before the first connect and ends after B receives and hashes the payload; digest equality is checked immediately afterward. It includes both handshakes, grants, payload generation/allocation, hashing, and C's streaming-scan instrumentation overhead. PKI setup and initial expected-hash generation happen before timing; cleanup is excluded. These are one-host loopback numbers, not WAN capacity or isolated-role CPU benchmarks.

### Capture and origin observations

C streaming-captures/scans **all decrypted outer application bytes**, including the admission acknowledgement and both directions of the forwarded inner TLS records. Per-connection/per-direction tails preserve scans across arbitrary read boundaries and interleaving. It does not persist ciphertext dumps or secret material. A test proves that the grant canary and full hash are literal strings in the inner plaintext before encryption (not hidden by JSON base64 encoding). Scans cover grant canary, full hash text and binary, and both Node addresses in text and packed IPv4 form. Any hit, including an accidental four-byte packed-IP collision in random ciphertext, is conservatively FAIL; no hits are suppressed. Absence is empirical evidence, **not a cryptographic proof**.

For each sender-side community sample, C's observed Node-origin set is exactly `{127.0.0.2}`; for receiver-side it is exactly `{127.0.0.3}`. All scans in the saved run are clear; O successfully reads/validates both grants and the full hash as intended. The unsafe fixture observes both `.2` and `.3`. `Hops` in each sample records every actual Node, C, O-direct and O-outer local/remote socket pair.

The collusion negative controls combine C's measured adjacent-origin observation with the opposite Node's own measured local origin. Both origin sets become known; `strong_privacy_condition_satisfied` is explicitly **false**. PASS means the negative control detected the privacy failure, not that strong privacy passed. This is a knowledge-union demonstration with a known shared test flow, not an internet traffic-correlation experiment.

## Open questions (not implementation approval)

1. **Is non-colluding/passive community visibility an acceptable weaker goal?** Default: no change to the product's stronger privacy promise; do not ship this route under it.
2. **What economic goal justifies the extra hop?** Default: reject operator-egress savings as a motivation; measured payload savings are zero and wire savings are negative.
3. **How would real admission, revocation propagation, audit and key rotation work?** Default: separate key/domain and operator-bound signed admission; resolve authenticated revocation delivery, delay budgets, key rotation and outage behavior in a separate design before any product work.
4. **How would route selection prevent both legs using one community or colluding operators?** Default: treat same-C both-legs as unsafe and refuse it; do not claim that separate communities or namespaces solve collusion.
5. **What remains untested?** Default: require separate-process/isolation checks, adversarial resource limits, canonical/strict credential parsing, concurrency/replay/refresh semantics, WAN and packet accounting, and independent security review if a weaker-goal proposal is pursued.
