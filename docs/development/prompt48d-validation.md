# Prompt 48D validation

Branch: `phase20/48d-node-relay-connector`. Result: **ARKSEC-004 closed** by MVP
E2E run `arknet-p43-test-795aafddced2` (17/17 PASS, 2026-11-04 AKST). The
attempts below are kept because each found a real defect.

## Verified component boundaries

- Agent forwards only its newest unexpired verified relay list on every
  add/refresh/seed path; no list means replacement with web-seed-only runtime
  authorization. Full `go test -race ./...` and `go vet ./...` pass in
  `node-agent/`, including the release manifest check.
- Transfer Go API/backend race suite and vet pass. Exact relay schema, canonical
  addresses, unique IDs/addresses/pins, expiry, and runtime-only forwarding are
  covered. Existing transfer URL policy is unchanged.
- Source-mounted product-image tests: 53 worker, connector, libtorrent-policy,
  and generated-firewall tests passed before the independent review fixes.
- Harness tests: 69 passed, including split text/binary address leaks, mandatory
  plaintext BitTorrent/BEP10 evidence, blocked-attempt counters, and fixed
  gateway host mapping. Deployment tests: 47 passed.
- Real product container startup: health HTTP 200; `/proc/1/status` shows UID/GID
  10001 and zero effective/bounding capabilities after firewall installation.
- Separate disposable Linux namespace proof: configured TCP destination reachable;
  a live canary on an unapproved port blocked; runtime unable to change iptables.

## First real E2E attempt

Run: `arknet-p43-test-6d053459086e`. Item 3 passed. Item 9 failed during transfer
container restart with Core stopped: `transfer_network_initialization_failed`,
then transfer API timeout. No relay privacy/fail-closed/revocation acceptance is
claimed from this run. The run was interrupted after the blocker was identified.

Root cause: Docker removes the DNS record for a stopped container. The firewall
initializer deliberately resolves all configured destinations before applying its
fixed allowlist, so a missing fixture DNS name prevents safe startup. The
acceptance topology already assigns fixed gateway addresses; its transfer
overlay now installs matching `extra_hosts` for Core/tracker/relay only (never
another Node). This preserves names while gateways are stopped without relaxing
the firewall or changing production authorization. A separate live Docker proof
confirmed both DNS removal and successful product startup with the origin offline
and the fixed mapping.

## Independent connector review

Read-only review found reproducible defects requiring fixes before acceptance:

1. Concurrent SSL socket read/write operations can corrupt bidirectional streams.
2. A 200 ms socket timeout also affects writes, dropping streams under ordinary
   backpressure.
3. Accepted seed sessions are not retained for standalone renewal after the
   registration moves to another relay.
4. Reordering identical relay entries unnecessarily closes active streams.

Regression tests and fixes are complete: a single-owner nonblocking TLS pump
retains partial writes in bounded queues, control renewals go through the control
owner, accepted streams retain their relay for standalone renewal, and equivalent
priority changes preserve streams. Parent reran all 63 source-mounted product
image tests successfully, including 26 connector tests. The revised E2E completed
with 15 passes and two failures (items 11 and 16); no acceptance result is claimed.
Two in-memory mutation checks (partial-write
loss and unbounded queues) were caught by the delegate; its remaining mutation
rerun was pending tool approval and is not reported as passed. TLS pin-before-grant,
no direct fallback, exact-origin web-seed exception, and bounded tracker forwarding
were confirmed by review.

## Second E2E attempt and root cause

Run `arknet-p43-test-e0744ecbfb66`: 15 PASS, 2 FAIL. Item 11 timed out (B never
had a peer) and item 16 had no relay flow to revoke.

Root cause, reproduced in the product image: with `proxy_type=socks5` this
libtorrent build opens **no TCP listener** (`listen_port()` is 0, no
`listen_succeeded_alert`, `127.0.0.1:6881` refuses connections). The seeder's
connector accepted relayed sessions but had nothing to dial. Isolated probes with
a fake relay passed only because they never exercised the seed's inbound side.

Fix: the worker runs a SOCKS5-only download session and a separate unproxied seed
session listening on loopback. Because the seed session can dial without the
proxy, the tracker bridge now strips every peer from seed announce replies
(strict bencode, malformed replies become a sanitized failure); the firewall
remains the backstop.

Evidence:

- New `relay_integration_test.py`: the real Go relay and tracker binaries plus
  two libtorrent sessions with the product settings; a 4 MiB private v2 object
  is advertised only via a relay-signed endpoint and downloaded through the
  relay. Passes; now part of `scripts/check-all.sh` worker tests.
- Mutation: reverting the seed session to the proxied settings fails the
  integration test ("seed session opened no listener") and both new Manager
  listener tests.
- Source-mounted product-image suite: 68 tests pass.

## Later attempts

| Run | Result | Finding and fix |
| --- | --- | --- |
| v3 | Relay transfer and revocation worked; items 11/16 rejected their evidence | Agent namespaces carry IPv6 (`::1`, ICMPv6); the capture parser refused it. It now parses IPv6 and scans those bytes too; IPv6 fragments fail closed and any non-loopback IPv6 dial counts as a direct attempt. |
| v4 | Item 11: a capture dropped packets; item 16: no flow at the single pre-revocation sample | Captures now write to a VM-local volume with a 32 MiB buffer and are copied out (0 drops is still required). Item 16 waits, with B's Agent still frozen, until relayed flow is clearly positive. |
| v5, v6 | Items 15/16 failed late on Node A | The Docker VM disk reached 99%: each capture's volume was kept until the run ended. Captures now remove their container and volume as soon as the file is copied. Connector refusals now return `invalid_request` with a fixed logged reason. |
| v7 | 16/17; item 11 failed on its second fixture | The worker rejected the 64 MiB fixture's metadata (`metadata_mismatch`). Not reproduced: the check accepts 32/64/96 MiB fixtures from the harness's own helper, and 40 bind-mount write/read cycles showed no stale reads. The worker now logs the non-secret metadata diagnostic on this rejection. **Open intermittent harness issue.** |
| v8 | **17/17 PASS** | — |

## Acceptance evidence (run `arknet-p43-test-795aafddced2`)

- **Item 11, relay-only transfer:** 33,581,357 bytes from A's libtorrent reached
  B only through the relay; B verified the SHA-256 with its Core web seed
  disconnected.
- **No addresses:** full-namespace captures (loopback included) of both Agents
  and both transfer containers held none of the 8 counterpart addresses as text
  or packed binary, including BEP 10 `yourip`. Every outgoing attempt was
  classified to an approved gateway; firewall denied attempts: 0.
- **All relays down:** peer bytes stopped immediately and stayed quiet for
  53.3 s with 0 late peer bytes, while 76,196,106 bytes still arrived from Core's
  web seed.
- **Item 16, revocation:** with B's Agent frozen, Core's push reached the relay
  at 1.74 s and closed B's live relayed socket; B's bytes stopped 1.36 s after
  revocation (bound 5 s) and stayed quiet for 51.3 s. After unfreezing, B became
  `REVOKED`, refused transfers with `409 node_revoked` and kept its media; Node A
  stayed `CONNECTED`.
- **Mutation:** removing peer stripping (`peers`, `peers6`) or the
  trailing-data check from the seed-reply sanitizer each fails the connector
  tests; proxying the seed session fails the real-binary relay integration test.

Not covered: the live-test VMs were not redeployed.
