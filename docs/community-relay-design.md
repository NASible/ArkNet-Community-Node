# Community relay design spike (Prompt 48E)

Status: **experimental; not approved for product integration**. This is a
standalone investigation of the [accepted community-relay conditions](private-relay-design.md#conditions-for-community-run-relays),
not an implementation of a community relay service. ARKSEC-004 was closed by
the operator-only relay in Prompt 48D ([validation](development/prompt48d-validation.md));
nothing here changes that result.

The scope of 48E is this document and throwaway code under
[`scripts/spikes/community-relay/`](../scripts/spikes/community-relay/). It
changes no product code.

## Questions and requirements

| ID | Question / requirement | Evidence required |
| --- | --- | --- |
| R1 | Can an individual community hop forward a session without seeing both Node origins? | Actual local/remote socket identities for both community placements; an unsafe shared-hop negative control. |
| R2 | Can grants and swarm identity remain confidential from that hop? | Node-to-operator pinned TLS inside the community tunnel; inspect the bytes available to the community after its own transport decryption. |
| R3 | Can Core admit/revoke a community without trusting it to disconnect? | Signed, expiring admission bound to its key and operator; operator-enforced refusal and live tunnel closure. |
| R4 | Can failure preserve the operator trust boundary? | Only a fixed admitted operator target; wrong-pin refusal before authorization; no direct-peer fallback. |
| R5 | Does this lower operator egress? | Payload conservation and separately measured operator socket-write bytes, not community byte counts or a claimed billing discount. |
| R6 | What is the throughput cost? | Repeated baseline and two-hop transfers with digest verification, measured from connection setup. |
| R7 | Does this remain safe if a community operator is also, or colludes with, the other Node? | Explicit adversary analysis; do not substitute a noncollusion assumption for ArkNet's untrusted-Node model. |

## Candidate path

Use **one community leg per transfer**, with the operator always on the payload
path. Either orientation is possible:

```text
Sender-side community:
  Node A ── C ── operator O ── Node B
  └── pinned inner TLS to O ──┘   └─ pinned TLS to O
           C ══ authenticated outer TLS ══ O

Receiver-side community:
  Node A ── operator O ── C ── Node B
  └─ pinned TLS to O       └── pinned inner TLS to O ──┘
                   O ══ authenticated outer TLS ══ C
```

All connections are initiated outward by Nodes/community services; arrows above
show the logical payload path, not TCP dial direction. A connector remains in
its transfer namespace, with libtorrent on loopback. The tracker still advertises
operator-issued virtual endpoints, never Node or community-client origins.
The operator joins the authorized seed and download legs as in the operator-only
design. This spike uses a synthetic byte stream, not libtorrent or the 48D product
path.

The community service has one fixed operator upstream. It is not a SOCKS server,
HTTP CONNECT service, arbitrary-target forwarder, tracker, grant verifier, or
swarm endpoint allocator. Its upstream mTLS credential authenticates the
community service **to the operator**. Inside that transport, the Node performs
its own TLS handshake, authenticating the operator with its pinned public key.
The community terminates only the outer transport and therefore handles opaque
inner TLS records, not Node authorization or peer payload plaintext.

### Visibility by role

| Observer | Sender-side community | Receiver-side community | Application contents |
| --- | --- | --- | --- |
| Node A | C, and authenticated O identity | O | Its own authorization and peer bytes; no Node B origin supplied by the transport. |
| Community C | A and O | B and O | Inner TLS records, timing, lengths, its own admission/transport identity. No plaintext Node grants, subjects, full info hash, virtual endpoint, or peer payload. |
| Operator O | C and B | A and C | Both authorized subjects, full info hash, virtual endpoint and peer payload. Accepted operator trust from the original design is unchanged. |
| Node B | O | C, and authenticated O identity | Its own authorization and peer bytes; no Node A origin supplied by the transport. |
| Core | Admission owner/key and route policy; normal existing Node enrollment information | Same | Core already knows the identities it authorizes. Community admission does not make Core blind. |

The Node behind C is hidden from O's **immediate TCP peer address**, not from
Core's enrollment records or a colluding network observer. A community learns
that its immediate client is using ArkNet. Content-size, timing and public-catalog
fingerprinting remain possible; encrypted fields do not promise unlinkability.

### Why sharing the same community on both legs is unsafe

```text
Node A ── C ── O ── C ── Node B
```

The two C appearances are one observer. It sees A and B even if every application
byte is encrypted. Two different relay IDs belonging to the same operator do not
fix this. An admission/route planner would have to enforce ownership separation,
not merely ID separation; the narrower proposed path avoids a second community
leg altogether. This is still not a solution to collusion.

### Collusion is a separate blocking limitation

If C is controlled by Node B, or shares its observations with B, it can reveal
A's origin to B. O being between C and B does **not** prevent C from reporting the
source address it legitimately observes. C can correlate volume/timing with B's
session; hidden grants do not erase that observation. The symmetric attack
applies when C is on B's leg and colludes with A.

Core vetting and banning the known owner of the opposite Node from a route
reduce obvious abuse but cannot prove absence of undisclosed ownership or
out-of-band collusion. A community may also observe both addresses across
unrelated transfers or other services. The per-path visibility table is not a
global anonymity guarantee.

Consequently, the last collusion claim in the original community conditions
cannot be justified by the proposed two-hop topology alone. Under ArkNet's
untrusted-Node threat model this is a reason **not to enable community relays**,
not a reason to silently weaken the condition.

## Core admission and revocation design

These are proposed protocol requirements, not shipped API names or configuration.

1. An administrator approves a community owner and key. Core publishes a bounded,
   versioned, expiring signed descriptor: community ID, owner grouping, endpoint,
   transport SPKI, allowed operator audience/pin, and admission epoch. Use a
   dedicated signing domain/key; do not reuse tracker, assertion, endpoint or
   per-relay revocation authority.
2. The operator validates Core's admission signature, audience, lifetime and
   transport-key binding. mTLS proves possession of that community key. Admission
   identifies the hop, not the Node and not a swarm.
3. Before sending its grant, the Node authenticates O through the opaque tunnel.
   Its encrypted route authorization binds the expected community ID/epoch (or
   explicitly the direct-operator route). O compares that binding with the actual
   authenticated transport. C cannot evade a revoked-hop check by forwarding the
   same inner session to O's anonymous direct listener.
4. Existing swarm authorization remains inside the inner TLS session, with seed
   versus download scope, full-hash binding, pairwise subject, expiry and renewal.
   C never receives a bearer authorizing it to fetch or serve an object.
5. Core pushes signed monotonic admission revocations to every allowed operator,
   retries until acknowledged and exposes delivery failures. O denies new
   admission and closes all live outer tunnels and nested sessions indexed by
   that community. No cooperation or acknowledgement from C is a security
   dependency. Expiry is the fail-safe when a push cannot reach O.
6. Admission expiry/epoch applies to live sessions too. Operator restart must not
   resurrect revoked admission: persist the relevant revocation/sequence state or
   require a fresh Core snapshot before accepting community transports.
7. When C fails, the connector may retry a currently approved operator directly,
   using the direct-route binding. If O is unavailable, peer transfer stops; the
   independently authorized web seed remains available. Never dial a Node origin,
   accept a community-advertised operator pin, or fall back to plaintext.

Bound admission lifetime, revocation delivery bound, clock skew, maximum tunnel
count and buffer sizes must be decided before implementation. Immediate removal
from a list alone is insufficient for a connected or malicious community.

The throwaway spike models Core signing and operator enforcement in one
process. It does not implement Core administration, durable outboxes,
distributed revocation fan-out or production renewal/recovery.

## Egress hypothesis: an extra forwarder is not offload

Consider a verified payload of N bytes sent from A to B:

| Path | Operator payload ingress | Operator payload egress | Community payload egress |
| --- | --- | --- | --- |
| A → O → B | N | N | 0 |
| A → C → O → B | N | N | N |
| A → O → C → B | N | N | N |

This is a conservation invariant of the operator cut, not a throughput estimate.
Moving the physical Node-facing socket to C changes **where O sends bytes**, not
whether O sends them. Encapsulation, handshakes and authentication can add wire
egress. Counting only O→Node sockets while ignoring O→C would manufacture a
saving.

Thus the accepted condition that this topology provide operator egress savings
was a hypothesis to test. The measurements below confirm the invariant: it does
not. Cheaper transit to C can
change a bill but is not a reduction in bytes. Caching, multicast, or an
operator-off-data-path design could change the invariant, but would be a different
architecture with fresh authorization, content-confidentiality and correlation
risks; none is smuggled into this spike.

## Measurements

Run (2026-11-05, Colima VM, Linux arm64, 2 vCPU, 2 GB):

```sh
python3 scripts/spikes/community-relay/run.py --mib 32 --reps 3
```

Sanitized output: [`scripts/spikes/community-relay/result.json`](../scripts/spikes/community-relay/result.json).
The container ran with `--network none --cap-drop ALL --read-only` as a
non-root user. All four roles run in **one process** on distinct loopback
addresses (A `127.0.0.2`, B `127.0.0.3`, C `127.0.0.4`, O `127.0.0.5`). They
are logical roles, not separate namespaces or hosts. Each transfer is a 32 MiB
synthetic stream with a verified SHA-256, timed from before connection setup.
Core admission and grants are in-process mocks using separate ephemeral Ed25519
keys. They do not use the product protocols.

### Throughput and operator egress (medians of 3)

| Topology | MiB/s | O payload egress (bytes) | O TLS write egress (bytes) | C TLS write egress (bytes) |
| --- | ---: | ---: | ---: | ---: |
| A–O–B (baseline) | 456.9 | 33,554,432 | 33,603,428 | 0 |
| A–C–O–B (sender-side community) | 361.2 | 33,554,432 | 33,605,376 | 33,651,281 |
| A–O–C–B (receiver-side community) | 337.1 | 33,554,432 | 33,695,466 | 33,606,030 |

Individual runs: baseline 458.5/456.9/391.8, sender-side 368.2/353.3/361.2,
receiver-side 337.1/292.1/344.4 MiB/s.

- **Operator payload-egress saving: 0 bytes in both community placements.**
  The operator sends the full payload in all three topologies. Only the
  destination of its writes changes (B directly, or C). Its TLS write egress is
  *higher* than baseline by 1,948 bytes (sender-side) and 92,038 bytes
  (receiver-side, nested TLS). The community also sends a full copy, so total
  relay egress roughly doubles.
- O TLS write egress counts every raw write outside all TLS layers: O→Node
  plus O→C. It excludes IP/TCP headers and retransmissions, so it is not an ISP
  billing counter.
- The community hop cost 21–26% of median throughput on this shared VM. The
  figures include handshakes, payload generation and hashing. They are relative
  CPU numbers, not WAN capacity.

### Visibility at each hop (actual socket addresses)

| Topology | Node origins C saw | Grant, full hash or other-side address in C's decrypted bytes | O read grants and hash |
| --- | --- | --- | --- |
| A–O–B | (no C) | n/a | Yes, as intended |
| A–C–O–B | `127.0.0.2` (A) only | None found in 33,603,937 bytes | Yes |
| A–O–C–B | `127.0.0.3` (B) only | None found in 33,603,962 bytes | Yes |
| A–C–O–C–B (negative control) | **Both** `.2` and `.3`: unsafe | None found | Yes |

C scanned every byte it could read after decrypting its own outer TLS, in
both directions and across chunk boundaries. It searched for the grant canaries,
the full info hash (text and binary), and the counterpart Node address (dotted
text and 4-byte packed). Any accidental packed-address match would fail the
check rather than be filtered out. These are scan results, not a cryptographic
proof. Inner TLS is what prevents C from reading the contents.

### Admission, revocation and failure checks (20/20 PASS)

| Check | Result |
| --- | --- |
| Wrong operator pin | Node aborts before any grant is sent |
| Tampered, expired or revoked admission | New community tunnel refused |
| Revocation of a community during an active tunnel | Operator closes the tunnel within 1 s; new attempts refused |
| Admission expiry during an active tunnel | Closed within 1 s; new attempts refused |
| Revoked C re-proxies a C-bound session to O's direct listener | Refused: route binding inside the inner TLS does not match the transport |
| C asked to connect to any target other than O | Refused (fixed upstream, not an open proxy) |
| All 9 transfers | Payload verified, C scan clear, egress conservation holds |
| Same community on both legs | Sees both origins: marked unsafe |
| Collusion: C's observed origin plus the opposite Node's own origin | Both origins known; **strong privacy condition not met** (both placements) |

The 1 s bound is in-process enforcement at the operator. It is not a
distributed revocation SLA.

## Verdict

**INVALIDATED for the combined goal; do not ship community relays.**

- **Holds (protocol):** in a single-community-leg path, no community relay
  sees both Nodes' addresses, the grants, or the info hash. The operator hop
  enforces admission, revocation and expiry without the community's
  cooperation. Fail-closed and pin checks hold.
- **Fails:** a community relay colluding with, or operated by, the opposite
  Node learns both origins. Operator egress does not decrease (measured
  saving: 0 bytes, with slightly more wire bytes). Two accepted conditions, the
  collusion bound and the "real" egress saving, are therefore not met.
- The operator-only first release stands unchanged.

## Options and recommendation

| Option | One passive community sees both origins on that path? | Protects against community/opposite-Node collusion? | Reduces operator payload egress? | Position |
| --- | --- | --- | --- | --- |
| Operator-only | No community observer | Does not add a community adversary | Baseline N | Keep existing first-release decision (shipped in 48D). |
| One community leg + O | No, for the isolated route | No | No, still N | Experimental only; no shipping approval. |
| Same community on both legs + O | Yes | No | No | Reject. |
| Community replaces O | Yes | No | Yes, by removing O from the path | Violates accepted operator-hop and trust conditions; reject. |

Recommendation (see [Verdict](#verdict)): the encryption/admission mechanism
works, but the combined privacy-and-offload proposal fails on measurement and
threat model. Keep operator-only relays.

## Open questions (not decisions)

1. **May community-to-Node collusion be excluded from the threat model?**
   Recommended default: **no**. Retain the untrusted-Node model and operator-only
   production transport; an honest-hop demo cannot approve a weaker guarantee.
2. **Is community transport useful without operator egress savings?**
   Recommended default: **do not add its complexity solely for offload**. Evaluate
   censorship reachability or cheaper operator transit separately if wanted.
3. **Should an alternative that actually moves O off the data path be explored?**
   Recommended default: defer to a separately approved design spike; do not alter
   the requirement that an operator remains between untrusted Nodes here.
4. **If a restricted deployment were approved, how is ownership separation enforced?**
   Recommended default: one community leg per transfer, explicit owner groups,
   no automatic community selection, and no claim of Sybil/collusion resistance.
5. **What would production admission/revocation limits be?**
   Recommended default for a future prototype: short-lived descriptors, operator
   fail-closed expiry, signed push with acknowledgement/retry and durable epochs;
   choose numeric bounds only after distributed failure testing. The local spike's
   closure latency is not a distributed SLA.
6. **Does the operator still permit direct-to-operator fallback?**
   Recommended default: yes, as already accepted, but never direct-to-peer and
   never without current operator pin/grant authorization.

No open question is treated as approved by completion of this experiment.
