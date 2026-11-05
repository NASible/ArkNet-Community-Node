# Private relay design (Prompt 48, ARKSEC-004)

Status: **accepted 2026-11-03** (D5; see [Decisions](#decisions-confirmed-2026-11-03)).
No product code changed in this spike. Implementation is Prompts 48A–48E in
[the Phase 20 prompt pack](development/hermes-phase20-prompts.md#phase-20e--private-relay-arksec-004).

## Problem

Today the private tracker advertises each complete peer's origin address (its
TCP source address), and Nodes connect to each other directly. So a
downloading Node learns the address of every Node it receives from, and a
seeding Node learns the address of every Node it serves. ArkNet's model treats
Nodes as independently owned and untrusted, so Nodes must not learn each
other's addresses. Prompt 47 also left a gap here: peer connections that are
already open survive revocation, because a seeder cannot recognise a revoked
peer.

ARKSEC-004 acceptance: *with default configuration, peers neither receive each
other's origin address nor exchange payload directly; relay failure does not
silently enable direct mode.*

## Requirements

| # | Requirement |
| --- | --- |
| R1 | No Node learns another Node's address by default (public or private). |
| R2 | Authorization is issued by Core and scoped to one swarm (one v2 info hash), as tracker grants already are. |
| R3 | The relay keeps as little metadata as possible: nothing persisted and no addresses or grants in logs. |
| R4 | Fail closed: if the relay is unavailable, transfers stop; nothing switches to direct connections. |
| R5 | Revocation ends live sessions, not just new grants. |
| R6 | Outbound-only Nodes: a seeder behind NAT needs no inbound port. |
| R7 | Community Nodes can run it: no kernel modules, `NET_ADMIN` or VPN client. |
| R8 | The bandwidth cost is understood and acceptable. |

## Options

### A. Core-operated relay (recommended)

A small dedicated service, deployed alongside the tracker and run by the ArkNet
operator, splices TCP streams between Nodes. Each Node runs a connector next to
its transfer service:

```text
 Node A (seeder)                         relay (Core operator)                      Node B (downloader)
 libtorrent ◀─127.0.0.1─ connector ══TLS══▶ verify seed grant           ◀══TLS══ connector ◀─SOCKS5─ libtorrent
                         "listen"            assign endpoint 100.x.y.z:p           "connect 100.x.y.z:p"
                                             splice A ⇄ B               (download grant, same hash)
```

- **Authorization:** the relay verifies the Core-signed tracker grants that
  already exist: a seed grant registers a listener and a download grant
  connects to it. Both are bound to one v2 info hash, a pairwise subject and an
  expiry of at most 5 minutes. The relay needs only Core's public key. No new
  credential type is needed.
- **Addresses:** a seeder is known only by a random virtual endpoint in
  `100.64.0.0/10` that the relay assigns at registration. The tracker would
  advertise that endpoint instead of the origin. The downloader's libtorrent
  connects to it through the local SOCKS5 connector, so neither libtorrent
  ever sees a real address.
- **Revocation:** a session ends when either side's grant expires, unless the
  connector renews it on its control channel. The relay also accepts a push
  from Core that revokes subjects and closes their sessions at once. Core can
  compute every subject of a revoked Node, because subjects are derived from
  Core's key, the Node ID and the info hash.
- **Fail closed:** the connector forwards only to relay endpoints. A SOCKS
  request for any other address is refused, and if the relay is down the
  request fails.
- **Cost:** every relayed byte crosses the relay twice: in from the seeder and
  out to the downloader. See [Bandwidth](#bandwidth).

### B. Volunteer relay Nodes

Nodes with public reachability relay for others.

- The relaying Node sees both peers' origin addresses, so the leak moves to an
  untrusted Node that can log it. That fails R1 under ArkNet's threat model.
- Revocation must be pushed to, and obeyed by, untrusted relays (fails R5 for
  a malicious relay).
- It spreads bandwidth cost across Nodes. That is its only advantage, and it
  only helps if volunteers exist.
- Not recommended as the default. It could become a later opt-in tier with
  Core-vetted relay operators.

### C. TURN-style relay (for example coturn)

A mature relay with Core-issued, time-limited credentials (the TURN REST API
pattern: HMAC over an expiry and a name).

- Credentials are per user, not per swarm: coturn cannot check an info hash
  (weakens R2).
- libtorrent does not speak TURN, and TCP relaying (RFC 6062) has few clients.
  Each Node would still need a custom connector, the same work as option A.
- Revocation needs coturn's admin interface; credentials otherwise last until
  they expire.
- Bandwidth cost is the same as option A.
- It adds a large, general-purpose server and no capability ArkNet needs. Not
  recommended.

### D. WireGuard mesh (Headscale/Tailscale with DERP, Nebula)

Every Node joins an overlay network.

- By default WireGuard peers connect directly and learn each other's endpoint
  (fails R1). Keeping addresses private means forcing every peer through a
  relay (DERP-only), at which point the mesh is just a relay with extra layers.
- Overlay addresses are stable across swarms. That would link a Node's
  activity across objects and undo the tracker's pairwise subjects.
- Authorization is network-level reachability, not swarm-scoped. ACLs would
  have to change with every grant (fails R2).
- Nodes need `NET_ADMIN`/TUN or a userspace WireGuard stack in the transfer
  container (fails R7).
- Revocation (removing the Node from the network map) is fast. That is its one
  strength.
- Not recommended.

Tor and I2P (libtorrent supports I2P) were not shortlisted: throughput is low,
the operational dependency is heavy, and they protect against Core too, which
ArkNet does not need.

### Comparison

| | A. Core relay | B. Volunteer relays | C. TURN | D. WireGuard mesh |
| --- | --- | --- | --- | --- |
| Peers learn each other's address | No (measured) | Relay Node learns both | No | Yes, unless DERP-only |
| Swarm-scoped Core authorization | Yes (existing grants) | Yes, if relays obey | No (per user) | No (network-level) |
| Relay operator sees | Both origins, subjects, hash, bytes | Same, but untrusted | Origins, usernames, bytes | Everything (control plane) |
| Fail closed | Yes (measured) | Depends on client | Client shim needed | Policy-dependent |
| Revocation cuts sessions | Yes: push, plus grant expiry (measured) | Only if relay cooperates | Admin API, or credential expiry | Network-map removal |
| Core bandwidth | 2× relayed bytes (in + out) | Spread across Nodes | 2× | 2× when relayed |
| Node requirements | Connector sidecar, no privileges | Same, plus public port on relays | Custom shim | TUN/`NET_ADMIN` |
| New code | Relay + connectors (692 lines of Go in the spike) | Same, plus trust system | Shim + coturn ops | Control plane + ACL sync |

## Measurements (proof of concept)

The spike is in [`scripts/spikes/private-relay/`](../scripts/spikes/private-relay/README.md).
It runs throwaway code: a Go relay, a seeder connector and a SOCKS5 downloader
connector, each relay leg TLS 1.3, between real libtorrent 2.0 peers
(`python3-libtorrent`, Ubuntu 24.04). The object is a 128 MiB private v2
torrent.

Each side is on its own internal Docker network, so the downloader cannot reach
the seeder at all. Only the relay network joins them.

Everything ran on one Colima VM (2 vCPU, 2 GB RAM, arm64), which also ran both
libtorrent peers and both connectors. The figures are therefore CPU-bound
relative numbers, not WAN numbers. Two full runs, three transfers each:

| Measurement | Run 1 | Run 2 |
| --- | --- | --- |
| Direct (no relay), MiB/s | 61.4, 116.7, 123.2 (median 116.7) | 131.4, 142.5, 102.9 (median 131.4) |
| Through the relay, MiB/s | 86.1, 90.3, 82.6 (median 86.1) | 103.8, 54.1, 70.7 (median 70.7) |
| 3 downloaders at once through one relay, aggregate MiB/s | 102.0 | 94.9 |
| Relay CPU per GiB relayed (both TLS legs) | 1.97–2.51 s | 1.99–4.33 s |

Throughput is measured from the first connection attempt, so the relayed
figures include connection setup through the connector and relay.

Relayed transfers reached 54–74% of direct throughput on the shared VM.
In the worst case, one relay core handles about 1 GiB per 4.3 s
(about 240 MiB/s, roughly 2 Gbit/s), so the relay's cost is bandwidth, not CPU.

Address checks (run 2):

| Check | Result |
| --- | --- |
| Peer addresses the seeder's libtorrent saw | `127.0.0.1` only (its own connector) |
| Peer addresses the downloader's libtorrent saw | `100.123.121.10` only (a relay virtual endpoint) |
| Seeder-side addresses (3) in the 25 MB the downloader side received from the relay | None |
| Downloader-side addresses (22) in the 0.8 MB the seeder side received | None |
| Both checks searched for | Each address as dotted text and as its 4-byte binary form. The payload was all zeros, so a match could not occur by chance. |

Session control:

| Check | Run 1 | Run 2 |
| --- | --- | --- |
| Download grant expires mid-transfer (15 s grant, 2 MiB/s) | Session closed by relay; last byte 0.2 s after expiry; reconnect refused | Last byte 0.8 s after expiry |
| Downloader revoked mid-transfer | Relay closed 2 connections; last byte read 0.58 s later | 1.14 s later |
| Bytes read after revocation | 4.6 MB | 6.1 MB |
| Seeder revoked | Its endpoint disappeared; its connector's control connection was closed | Same |
| SOCKS request to the seeder's real addresses | Refused (SOCKS code 2) | Refused |
| Relay stopped | SOCKS request fails (code 1); downloader receives 0 bytes in 15 s | Same |

The bytes read after revocation were already in the downloader's own socket
buffers: the rate-limited downloader reads slowly. The relay forwarded nothing
after it closed the session.

### Findings that the design must carry

1. **libtorrent tells each peer the address it sees it at** (BEP 10 extension
   handshake, `yourip`). In the first attempt the seeder's connector was a
   separate container on the seeder's private network. The downloader then
   received that connector's address, a seeder-side address. With the
   connector in the transfer service's network namespace, dialing `127.0.0.1`,
   the value is always `127.0.0.1` and reveals nothing. The product connector
   must run in the transfer container's network namespace or inside the
   transfer service itself.
2. **uTP bypasses the relay.** libtorrent tries uTP (over UDP) first, and a
   SOCKS5 TCP proxy does not carry it. Relayed sessions must set
   `enable_outgoing_utp` and `enable_incoming_utp` to false.
3. **All relayed peers share one source address** (the connector), so seeders
   need `allow_multiple_connections_per_ip`.
4. **Listening sockets are fixed at startup:** libtorrent creates one per
   interface present when it starts (also in the swarm-verification notes).
5. **The tracker must stop advertising origins** and advertise relay endpoints
   instead. Otherwise libtorrent learns direct addresses even if it never uses
   them. That also requires the transfer container's network egress to be
   limited to the relay, tracker and web seed, so libtorrent cannot reach a
   peer directly even if it learns an address.

## Bandwidth

Every relayed byte enters and leaves the relay. Compared with the Core web
seed, which sends each byte once, a Core-operated relay costs Core the **same
egress** (plus ingress, usually free) per byte delivered. So with the relay as
the default, peer-to-peer transfer no longer reduces Core's egress.

It still provides:

- sources for objects Core no longer stores, or stores on slower or colder
  storage;
- capacity when the web seed is overloaded or down;
- relay capacity that can sit on hosts with cheaper or unmetered egress than
  Core's origin.

It no longer provides bandwidth offload. That is the main trade-off for you to
decide (open question 2).

## Metadata the relay holds

The relay holds in memory only: active registrations and sessions (pairwise
subject, info hash, virtual endpoint, expiry), revoked subjects (until their
grants could have expired), and aggregate counters.

It necessarily sees both Nodes' origin addresses (TCP), as Core already does
through heartbeats and the tracker. It also sees which subjects exchange which
object, and the byte counts. While a session is live, those addresses link a
Node's pairwise subjects across swarms. So the relay must log nothing about
peers, like the tracker, and must not be readable by Nodes.

Payload is visible to the relay inside each TLS leg. This adds no exposure,
because Core already serves every catalog object from its web seed.
End-to-end encryption between connectors is possible but not proposed (open
question 6).

## Recommendation

**Option A: a Core-operated relay as the default peer transport**, with:

- a separate minimal service (like the tracker), deployable to more than one
  operator-owned host, verifying existing tracker grants with Core's public
  key;
- a Node-side connector in the transfer container's network namespace: a
  SOCKS5 outbound side for libtorrent, and a listen side dialing
  `127.0.0.1`;
- tracker responses carrying relay virtual endpoints only, never origins;
- the transfer container's network egress limited to the relay, tracker and
  web seed; uTP, DHT, LSD and PEX off;
- sessions bounded by grant expiry, with renewal on the control channel, plus
  an immediate revocation push from Core;
- direct peer connections only as an explicit, Core-authorized opt-in on both
  sides, never as an automatic fallback (ARKSEC-004).

This closes ARKSEC-004's address exposure and the open-connection part of the
Prompt 47 revocation window. A revoked Node's live relayed sessions end when
Core pushes the revocation; without the push, they end when the current grant
expires (at most 5 minutes).

## Decisions (confirmed 2026-11-03)

| # | Question | Decision |
| --- | --- | --- |
| 1 | Who operates relays (D5) | **The ArkNet operator runs most relays**, on operator-owned hosts. **Community-run relays are allowed only where they cannot compromise security** (conditions below). The first release is operator-only; community relays come in a later prompt with their own spike. |
| 2 | Bandwidth | Accepted: relay egress costs about the same as the web seed. |
| 3 | Direct mode | Not in the first release. If added later, both Nodes' operators must opt in and Core records the consent. Never an automatic fallback. |
| 4 | Relay and tracker | Separate services. The tracker stays minimal; the implementation prompt settles how it learns relay endpoints. |
| 5 | Revocation push | Required: Core pushes revocations to every relay, which closes the revoked Node's live sessions at once. |
| 6 | Relay sees payload | Accepted. Each leg is TLS. |
| 7 | Several relays | Core publishes a signed relay list; Nodes try relays in order. If none is reachable, peer transfers stop (fail closed). The web seed is unaffected. |

### Conditions for community-run relays

A single-hop community relay would see both Nodes' origin addresses, which
is exactly the leak ARKSEC-004 removes. A community relay is therefore
acceptable only if all of these hold:

- **No relay outside the operator's control sees both Nodes' addresses.** A
  community relay may only be one hop of a two-hop path whose other hop is an
  operator relay, so it sees one Node's address and the operator relay's
  address, never both peers.
- **It cannot read grants or swarm identity.** The connector's session to the
  operator relay (grant, info hash, endpoint, payload) runs end to end
  through the community hop, encrypted to the operator relay. The community
  relay forwards ciphertext and learns only "this address uses ArkNet" and
  byte counts.
- **Core admits it.** Community relays appear only in Core's signed relay
  list, are approved per operator, and are removed (with immediate session
  cuts at the operator hop) on revocation.
- **It cannot weaken fail-closed.** If the community hop fails, the
  connector uses an operator relay directly; it never connects to a peer.
- **Measured before shipping.** The two-hop path needs its own spike: address
  checks at every hop, throughput, and that the operator hop's egress saving
  (the community hop carries the Node leg) is real.

Collusion between a community relay and a Node is bounded by the first
condition: the operator hop is always between them.

**48E qualification:** the two-hop topology alone does not establish that
collusion claim, nor does moving a Node leg to a community relay establish an
operator-egress saving. The [community relay design spike](community-relay-design.md)
examines both claims explicitly. Community relays remain experimental and are
not approved for product integration; the accepted operator-only first release
is unchanged.

## Questions as asked


1. **D5, the operator:** do you accept a Core-operated relay as the default?
   Where should it run? For example, your existing ingress hosts, which would
   keep traffic on infrastructure you own.
2. **Bandwidth:** the relay costs Core the same egress as the web seed. Is that
   acceptable, or must peer-to-peer also reduce Core egress? The latter means
   volunteer relays or direct mode, and both weaken address privacy.
3. **Direct mode:** should explicit direct connections exist at all? If so,
   must both Nodes' operators opt in, and should Core record that consent per
   Node or per swarm?
4. **Relay and tracker:** merge them into one service (simplest: the tracker
   already holds the swarm table), or keep them separate, with the relay
   telling the tracker which endpoints exist?
5. **Revocation push:** is an immediate Core→relay push required, or is
   expiry within the 5-minute grant lifetime acceptable as a first step?
6. **Payload visibility:** is the relay (the Core operator) seeing payload
   acceptable, given that Core already holds every catalog object? Or do you
   want end-to-end encryption between connectors?
7. **Availability:** with several relays, how should a Node choose one?
   Should transfers simply stop when no relay is reachable (fail closed,
   while the web seed still works)?
