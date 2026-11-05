# Node revocation

When ArkNet Core revokes a Node (`REVOKED`/`BLOCKED`), Core stops issuing it
anything new at once: heartbeats, catalog reads, transfer and seed grants, and
login assertions are all refused. Grants it already holds (tracker, web seed
and torrent metadata URLs) stay valid until they expire, at most 5 minutes. This page describes what
a **cooperative** Node does with transfers already in progress, the bound on
that, and what a **malicious** Node that ignores revocation can still do
(ARKSEC-006; Prompt 47).

## Cooperative Node

1. **Learning of it.** The Agent's next heartbeat is refused. The Agent then
   asks Core for its signed federation status and, on an authoritative
   `REVOKED/BLOCKED` answer, persists the terminal `REVOKED` state
   (`node-agent/internal/heartbeat/worker.go`). That takes at most one
   heartbeat interval (`ARKNET_HEARTBEAT_INTERVAL`, default 30 s, at most 5 min)
   plus the time of two Core requests.
2. **Stopping transfers.** Entering `REVOKED` wakes the transfer and seed
   reconcilers immediately instead of at their next interval
   (`node-agent/revocation.go`). The transfer reconciler
   (`transfer_finalizer.go`) then:
   - cancels every `DOWNLOADING` or `PAUSED` transfer, including any it
     cannot match to a selection; libtorrent removes the torrent and announces
     `stopped` to the tracker;
   - marks interrupted selections `ERROR`;
   - asks Core for nothing (no catalog, no grant refresh);
   - leaves finished transfers and every `AVAILABLE` file in the media
     directory untouched, so verified local media keeps playing in Jellyfin.

   The seed reconciler unseeds everything, because it serves only while the
   Node is `CONNECTED`. Both checks run on every pass, so a Node that restarts
   while `REVOKED` (when the transfer service restores its torrents) cancels
   them again on its first pass.
3. **Refusing new work.** Transfer `start` and `resume`, from the operator API
   and from automatic media-miss handling, answer `409 node_revoked` before
   anything is sent to the transfer service or Core. If the Node state cannot
   be read they answer `503 local_state_unavailable`. `pause` and `cancel`
   stay available.

**Bound:** a cooperative Node's transfer byte counters stop within one
heartbeat interval plus 10 s of Core's decision (40 s with the default 30 s
interval). MVP E2E item 16 measures this with a 5 s interval and a 15 s bound
(see [Prompt 47 validation](development/prompt47-validation.md)).

## Tracker

The private tracker holds no Node identities and has no connection to Core,
so it cannot learn of revocations directly. It drops a revoked Node's record:

- **cooperative Node:** at the Node's own `stopped` announce, sent when its
  Agent cancels the transfer or removes the seed (seconds after the Agent
  enters `REVOKED`);
- **any Node:** at the first announce in that swarm after the Node's tracker
  grant expires. Core issues grants for 5 minutes and refuses to renew them
  after revocation, so this happens no later than 5 minutes after Core's
  decision. Expired records are removed before admission and are never
  advertised (`tracker/internal/tracker/handler.go`; tests
  `TestStoppedAndStalePeersAreRemoved`,
  `TestPeerIsRemovedWhenGrantExpiresBeforePeerTTL`).

Downloading (incomplete) peers are never advertised to anyone, so only a
revoked Node that was *seeding* could still be handed to other peers during
that window.

## Malicious Node: residual window (ARKSEC-004)

A Node that ignores revocation keeps whatever it already holds. Measured from
Core's decision:

| Channel | What still works | Bound |
| --- | --- | --- |
| Core federation API (catalog, new grants, heartbeats, login assertions) | Nothing | None: refused at once |
| Core torrent metadata | Fetching it with its last grant | Until the grant expires: at most 5 min |
| Tracker | Announcing with its last grant; if that grant includes `seed` and the Node reports complete, being advertised to other peers | Until the grant expires: at most 5 min |
| Core web seed | New range requests with its last grant | Until the grant expires: at most 5 min |
| Core web seed | A range request that started before the grant expired | Until that response ends: Core's HTTP write timeout ends every response after 30 s, so at most 30 s past the grant's expiry |
| Peer connections (relayed) | Relayed sessions already open | Core pushes the revocation to every relay, which closes the Node's live sessions: measured 1.74 s from the decision, bytes stopped in 1.36 s (MVP E2E `arknet-p43-test-795aafddced2`, item 16) |
| Peer connections (new) | Opening a relayed session with its last relay grant | Until that grant expires: at most 5 min, and refused once the relay has applied the push |
| Peer connections (direct) | Nothing | Nodes never learn each other's addresses (trackers advertise only relay endpoints), so there is no address to dial |

ARKSEC-004 is closed by the relay (Prompt 48D). The remaining malicious-Node
windows are the grant lifetimes above. Core's web seed still does not check
Node status per request, so an open web-seed response can outlive revocation by
up to its grant expiry plus 30 s.
