# Federation Heartbeats

Approved Nodes send signed outbound heartbeats. No operator identity or public
node directory is introduced.

## Wire contract and privacy

`POST /api/federation/v1/nodes/{node_id}/heartbeats`

```json
{
  "node_id": "<persisted opaque node ID>",
  "version": "0.0.1-dev",
  "state": "CONNECTED",
  "health": {"healthy": true, "database_healthy": true},
  "capabilities": {"peer_serving": false}
}
```

Only these five fields are accepted, with exact unique names. State must be a
known Node lifecycle state. Health is only two booleans, not diagnostics or
resource inventories. Capabilities contain only the local Peer Serving opt-in. No IP metadata,
operator hostname, MAC, serial number, filesystem data, user data, private key
or administrator token is sent. Transport peers can still observe source IPs;
this is not transport anonymity. Reports are **node assertions**, not independent
Core health attestation: a malicious approved operator can lie about its node.

The existing persisted Ed25519 credential signs the exact method, escaped path,
body digest, canonical timestamp and node ID. Core verifies its pinned key and
requires approved/trusted enrollment. One node's key cannot authenticate another.

Unlike read-only enrollment status, heartbeats mutate liveness data. Core stores
and atomically enforces an increasing proof timestamp per node. Replaying a
captured heartbeat cannot refresh last-seen or overwrite newer state/version,
even after Core restart. Proof timestamps must also satisfy the existing
one-minute clock-skew window. Keep Core and Nodes' clocks synchronized. This is
heartbeat-specific authorization, not a reusable write session for future APIs.

A successful response is exactly:

```json
{"node_id":"<same node ID>","accepted":true}
```

Core uses its own database clock for `last_seen_at`; it does not accept client
last-seen timestamps. Version, current state and the bounded health summary are
updated atomically without changing enrollment or trust. Invalid payloads,
failed authorization, stale/replayed proofs and database failures do not update
liveness. The accepted Peer Serving value is stored atomically with the other
heartbeat metadata and gates future source eligibility. Responses do not expose
SQL errors or credential material. See [Opt-In Peer Serving](peer-serving.md).

## Scheduling and lifecycle

`ARKNET_HEARTBEAT_INTERVAL` defaults to **30s**, accepts **5s through 5m**, and
is passed through the Node Compose stack. Heartbeats begin only after approved
federation enrollment, reusing the stored credential and Core URL. Requests are
bounded and cancelable, retain normal TLS verification, and do not follow
redirects or use environment HTTP proxies.

- A successful attempt resets failure backoff to the configured interval.
- Failure delays start at the configured interval, then double after each
  consecutive failure, capped at five minutes, with bounded positive jitter.
- A first failed Core attempt moves a connected/active node to `DEGRADED`.
- Three consecutive failed attempts move it to `OFFLINE`.
- Retrying from degraded/offline goes through `RECONNECTING`; a verified success
  persists `CONNECTED` and resumes the normal cadence.
- Recovery does not reset enrollment, create a new credential or re-enroll.
- Success does not regress an already active `SYNCHRONIZING` or `MESH` state.
- Heartbeats do not revive quarantined, revoked or error states.

Transitions use the existing persisted compare-and-swap state machine and audit
history. The local API continues serving during Core outages, and `/health`
continues to measure the local Agent/database rather than Core reachability.

Local `core_connectivity` is an observed heartbeat status: `UNKNOWN` before a
result after worker startup and the corresponding connected/degraded/offline/
reconnecting status thereafter. Disabled federation retains `NOT_IMPLEMENTED`.
Connection observation is not a promise of future reachability. The first
successful recovery packet reports `RECONNECTING`; the next normally scheduled
heartbeat reports `CONNECTED`, avoiding an extra immediate write.

On shutdown the federation worker is canceled and joined before PostgreSQL is
closed. Temporary Core failures never terminate the Agent. Persistent enrollment
and credentials survive restart; ordinary heartbeat observations are refreshed.

## Viewing Core's current report

The development administrator authorization mechanism remains opt-in and
separate from node credential authentication. With the existing Core admin
Bearer authorization, request:

`GET /api/admin/v1/nodes/{node_id}`

The detail response includes node ID, enrollment/trust, reported version, current
state and last-seen timestamp. Nodes with no accepted heartbeat have null state
and last-seen. This endpoint is administrative, not a global directory exposed
to Node operators. No Core admin UI is implemented.

## Verification

From repository root:

```sh
(cd core && go test ./... && go test -race ./... && go vet ./...)
(cd node-agent && go test ./... && go test -race ./... && go vet ./...)
python3 scripts/check-heartbeat-stack.py
```

The isolated live check uses a five-second interval for runtime, generated
secrets, two private PostgreSQL databases and a test-only application network.
It verifies fresh Core metadata, stops Core, observes persisted degraded/offline
states while the local API stays healthy, restores Core, verifies automatic
recovery without re-enrollment, and restarts the Agent. It also runs real
PostgreSQL tests. Only its uniquely named resources are removed; existing `.env`,
services and development databases are untouched. Run live scripts sequentially.

The previous handshake check now asserts **completed enrollment persistence**
during a Core outage, not permanently `CONNECTED`: heartbeats legitimately
change connection state. This does not weaken credential persistence or the
no-re-enrollment requirement.

See [automatic Core reconnect](reconnect-behavior.md) for the complete
`MESH → DEGRADED → OFFLINE → RECONNECTING → CONNECTED` outage path, structured
transition logs, and automatic read-only UI refresh behavior.
