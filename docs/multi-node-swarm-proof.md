# Multi-node private swarm proof (Prompt 27)

A disposable Core, Node A and Node B prove that a Node can obtain verified pieces
from both an opted-in peer and the Core HTTP web seed. The same run shows that
Core egress drops compared with a Core-only download, and that the Node still
completes when the peer disappears mid-transfer.

```sh
python3 scripts/check-multi-node-swarm-stack.py --results /path/to/result.json
```

The run takes about 10 minutes. It creates only resources with a random
`arknet-p27-test-*` prefix and verifies that it removed them exactly.

## What the harness does

1. Builds Core (with private tracker and web seed), and builds Node A and Node B
   with the real libtorrent transfer service. Each Node is on its own isolated
   network. DHT, PEX and LSD are disabled everywhere.
2. Generates a playable 40 s H.264/AAC MP4 and a **private, v2-only,
   multi-piece** torrent (256 KiB pieces), then registers it in the Core
   catalog.
3. **Baseline:** Node A replicates the object through its real operator path
   (select → transfer → finalize → `AVAILABLE`) with only the Core web seed.
   A metering proxy in front of Core counts the payload bytes of successful web-seed responses only.
   Torrent metadata is excluded from the count. The proxy throttles to 96 KiB/s so
   the phases are observable.
4. Enables Peer Serving on Node A over its real local API, then waits for the
   heartbeat and for Core's `ListEligibleMediaSources` predicates to include
   Node A.
5. Starts the Node A peer adapter (see below) and resets the meter. Node B then
   requests the same object through its real operator path.
6. Once Node A has uploaded ≥ 2 MiB while Node B is still incomplete, the
   adapter container is **stopped**. Node B must finish from Core alone.
7. Verifies Node B's file SHA-256 matches the source, then validates the
   evidence (below) before writing the sanitized result.

## Result

Run `arknet-p27-test-22764207e3fb`, 2026-10-25. Node B's object matched the
source SHA-256 (`8492180b0264e055…`).

| Measurement | Value |
| --- | --- |
| Object size | 26,274,725 B (25.1 MiB) |
| Core egress, Core-only download (Node A baseline) | 28,273,573 B |
| Core egress, Node B two-source download | **23,199,744 B** (−17.9 %) |
| Core egress before Node A disappeared | 884,736 B |
| Core egress after Node A disappeared | 22,315,008 B |
| Node A upload to Node B (verified payload) | **4,727,020 B** |
| Node B verified progress when Node A was stopped | 2,911,141 B (11 %) |
| Node B completion time | 254.8 s |
| Node B average transfer rate | 103,102 B/s (≈ 100.7 KiB/s) |

The test pacing limits Core to 96 KiB/s and Node A to 256 KiB/s. The rate and
saving reflect where Node A was stopped, not production throughput. Node A
served alone for most of the overlap; the saving grows with how long the peer
stays.

The baseline exceeds the object size by about 7.6 % because libtorrent
re-requests some web-seed ranges. It is the honest Core-only cost under the
same conditions.

## Acceptance criteria

| Criterion | Evidence |
| --- | --- |
| Node A contributes verified pieces | Node A uploaded 4.73 MB of payload. Node B accepts only pieces that pass the v2 Merkle hash check, and the completed file matches the source SHA-256. |
| Core egress lower than Core-only | 23.20 MB vs 28.27 MB baseline. |
| Node B completes if Node A disappears | Node A was stopped at 11 % verified progress. Core then sent 22.32 MB and Node B completed with an exact hash. |

## Evidence validation

`scripts/multi_node_swarm_evidence.py` rejects a run unless all of the
following hold:

- Node A uploaded a positive amount of payload;
- Core egress is below the baseline;
- Node B completed the exact object size;
- Node A was stopped strictly mid-transfer;
- Core sent payload after Node A was stopped;
- **byte accounting:** Node A upload + Core egress ≥ object size.

This run: 4,727,020 + 23,199,744 = 27,926,764 ≥ 26,274,725.

The byte-accounting check is what makes a pass meaningful. An earlier run passed
every threshold while a stray seed silently served Node B. A per-phase variant
("Core after the stop ≥ object minus progress at the stop") was tried and
dropped as unsound: verified progress lags bytes already delivered by Node A. The
adapter's upload counter is read again after the stop so in-flight bytes are
counted.

## Disclosures

- **Node A serves through a test-only adapter**, not product code. ArkNet has no
  peer-serving data path yet: Prompt 26 added only the opt-in setting, the
  heartbeat report, and Core source eligibility. The adapter is a libtorrent
  seeder in its own container. It reads Node A's verified media volume read-only
  and announces to the private tracker with a Core-format seed grant. Node B's
  side (grant, tracker, web seed, verification, finalization) is entirely product
  code.
- The adapter sets `ignore_limits_on_local_network: false`. Without it,
  libtorrent ignores rate limits between peers on the Docker bridge, and Node A
  would send almost everything before it could be stopped.

## Product bugs found and fixed by this proof

1. **Core web seed 404'd for most media** (`core/web_seed.go`). libtorrent
   loads a non-piece-aligned v2 file with a tail pad file and requests
   `<web_seed_url>/<torrent name>`. Core now accepts exactly that one extra
   segment, only when it equals the media ID. See
   [Core web seed](core-web-seed.md).
2. **The transfer worker dropped its own web-seed connection every second**
   (`transfer/worker.py`). Re-applying sources on each status refresh
   disconnected in-flight requests. Sources are now secured once per handle.
3. **Completed downloads kept seeding** regardless of the Peer Serving setting,
   which violated Prompt 26's opt-in. The worker now removes completed torrents
   from the session.

Each fix has a regression test.

## Not in this phase

A product peer-serving data path (Agent-managed seeding of retained replicas
with seed grants), and Core source selection driving which peers a downloader
is given. The private tracker currently provides peers.
