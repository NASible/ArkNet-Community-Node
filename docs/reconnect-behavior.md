# Automatic Core Reconnect

Approved Nodes recover from temporary Core outages without operator action or
enrollment reset. This behavior extends the authenticated heartbeat lifecycle; it
does not introduce a second connectivity mechanism.

## Lifecycle

An active node follows the persisted state graph:

```text
MESH → DEGRADED → RECONNECTING → DEGRADED → RECONNECTING → OFFLINE
OFFLINE → RECONNECTING → CONNECTED
```

The repeated `RECONNECTING` states are real bounded attempts, not a tight loop.
The first failed heartbeat records `DEGRADED`; the third consecutive failure
records `OFFLINE`. A successful authenticated heartbeat records `CONNECTED` and
resets the failure counter and cadence. A future mesh/session layer may move it
through `SYNCHRONIZING` to `MESH`; reconnect does not fabricate a mesh session.
Thus the externally summarized flow is:

```text
MESH → DEGRADED → OFFLINE → RECONNECTING → CONNECTED/MESH
```

## Availability and persistence

Core availability is not part of local Agent health. During an outage:

- the local Agent API and UI proxy remain available;
- the private Node PostgreSQL database remains available;
- node identity, private credential and approved enrollment are unchanged;
- persisted state and transition history remain readable;
- the heartbeat worker contains failures instead of terminating the Agent.

Core restart requires no Node action. The next scheduled retry uses the original
node-bound credential and enrollment. Core receives a `RECONNECTING` report,
then the normal next heartbeat reports the durable recovered state.

Retries begin no sooner than the configured heartbeat interval, double after
consecutive failures, include bounded positive jitter, and cap at five minutes.
Requests themselves are bounded and cancelable. A persisted `OFFLINE` state is
retained across Agent restart when Core remains unavailable.

## Observability

Every successful heartbeat-driven state mutation emits one structured Agent log:

```json
{"msg":"node_state_transition","from_state":"OFFLINE","to_state":"RECONNECTING"}
```

The event contains state names only—no node ID, credential, request body,
database URL, token, or raw error. Failed compare-and-swap attempts are not
reported as completed transitions.

The read-only Node UI refreshes `/api/v1/status` every five seconds after its
initial successful load. Background refresh does not replace a usable dashboard
with a loading screen, requests are bounded, and timers/requests are canceled on
unmount. The displayed Node state and Core connectivity therefore track
`DEGRADED`, `OFFLINE`, `RECONNECTING`, and recovery without manual refresh.
The complete local-versus-federation presentation and safe status contract are
documented in the [functional Node dashboard](node-dashboard.md).

## Verification

From the repository root:

```sh
(cd node-agent && go test ./... && go test -race ./... && go vet ./...)
(cd node-ui && npm test && npm run build)
python3 scripts/check-heartbeat-stack.py
```

The disposable live test establishes a real approved node, advances it through
the state-machine boundary to `MESH`, stops Core, observes degraded/offline state
while Agent, database, and UI proxy remain available, restarts Core, and verifies
automatic authenticated recovery. It asserts structured transition logs, fresh
Core last-seen metadata, unchanged enrollment and pinned credential, Agent
restart continuity, real PostgreSQL tests, and removal of only uniquely named
disposable resources.
