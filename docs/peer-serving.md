# Opt-In Peer Serving

Peer Serving is a local Node Operator setting. It is **off by default** on fresh and upgraded Nodes. The Node Agent persists the setting in its local PostgreSQL `node_configuration` record and exposes it only through the local management API:

- `GET /api/v1/settings/peer-serving`
- `PUT /api/v1/settings/peer-serving` with the exact JSON body `{"enabled":true}` or `{"enabled":false}`

The ArkNet UI **Settings** page is the supported operator control. Enabling it is an explicit opt-in; disabling it does not remove retained media or disable downloads.

## Core participation report

Every authenticated heartbeat contains the exact capability object:

```json
{"peer_serving":false}
```

Core stores the latest accepted value as reported participation. A fresh Core node record also defaults to false. Replica availability and serving participation remain separate facts: a Node may retain and report an ArkNet-managed object while declining to serve it.

Core source selection must use `ListEligibleMediaSources`, which requires all of the following at query time:

- the catalog object is active;
- the replica is known;
- the Node is approved and trusted;
- the Node's latest accepted `peer_serving` report is true.

A Core `REMOVED` tombstone therefore stops advertisement immediately, even if
the replica row and the opt-in remain. See [Removed media](removed-media.md).

Consequently, an accepted heartbeat reporting false removes the Node from subsequent source-selection results without deleting its replica record. Re-enabling serving makes retained, known replicas eligible again after Core accepts the updated heartbeat.

## Receiving while serving is off

The setting does not affect media selection, download grants, tracker download
authorization or the Core HTTP web seed. A Node with Peer Serving off can still
receive and verify content.

Receiving never implies serving:

- **Finished downloads:** a completed transfer leaves the swarm.
- **Downloads in progress:** the private tracker only advertises peers that
  announce a complete copy with a seed grant. So no peer learns the address of
  a Node that is still downloading, and nobody can request its partial pieces.
  See [Private tracker](private-tracker.md).

## Seeding (product path)

The Node Agent's seed reconciler runs on the transfer reconcile interval
(`ARKNET_TRANSFER_RECONCILE_INTERVAL`). On each pass it decides which objects
this Node serves.

It serves an object only while **all** of these hold:

- Peer Serving is ON.
- The Node's local state is `CONNECTED` and its enrollment identity is
  `APPROVED`.
- The object's local copy is `AVAILABLE`, the object is `ACTIVE` in Core's
  catalog with a matching size and a v2 hash, and the copy is not in
  `VERIFICATION_FAILED`.
- Core issues a seed-only grant (`{"actions":["seed"]}`) for it. Core does this
  only for an eligible source: a known replica, Peer Serving reported on, an
  approved and trusted Node, and an ACTIVE object. The grant must include
  torrent metadata and must never include a web seed.

For each object that qualifies, the Agent asks the transfer service to seed
(`POST /api/transfer/v1/seeds`). The service hash-checks every piece before
serving, in upload-only mode, from the Agent-finalized file. Grants are
refreshed at half their lifetime.

The Agent unseeds (`DELETE /api/transfer/v1/seeds/{id}`) within one interval in
any of these cases:

- Peer Serving is turned OFF, or the setting cannot be read.
- The Node is not `CONNECTED`, including `REVOKED`, `QUARANTINED`, `DEGRADED`
  and `OFFLINE`, or its identity is not approved.
- The object is removed in Core or is no longer `AVAILABLE` locally.
- Core refuses the seed grant (401 or 403).
- Core cannot be reached and the object's current grant has expired. A
  transient failure keeps an existing seed only until then. The tracker also
  drops the peer when its grant expires.

Seeds are never persisted. After a restart, the Node serves nothing until a
fresh grant is obtained.
