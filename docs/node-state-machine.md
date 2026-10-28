# Node State Machine

The Agent owns a deterministic, explicit persisted state machine. Enrollment and
heartbeat workers use its validated compare-and-swap transitions; no HTTP mutation
endpoint is exposed. States describe the local lifecycle. `CONNECTED` records the
most recent verified heartbeat outcome, not guaranteed future reachability;
`QUARANTINED` is not network enforcement.

## Transition policy

Only the following directed transitions are allowed. Everything else—including
self transitions, unknown values, and different casing—is rejected.

| Current state | Allowed next states |
| --- | --- |
| `PROVISIONING` | `ENROLLMENT_PENDING`, `ERROR` |
| `ENROLLMENT_PENDING` | `CONNECTED`, `OFFLINE`, `QUARANTINED`, `REVOKED`, `ERROR` |
| `CONNECTED` | `SYNCHRONIZING`, `DEGRADED`, `OFFLINE`, `QUARANTINED`, `REVOKED`, `ERROR` |
| `SYNCHRONIZING` | `MESH`, `DEGRADED`, `OFFLINE`, `QUARANTINED`, `REVOKED`, `ERROR` |
| `MESH` | `SYNCHRONIZING`, `DEGRADED`, `OFFLINE`, `QUARANTINED`, `REVOKED`, `ERROR` |
| `DEGRADED` | `RECONNECTING`, `OFFLINE`, `QUARANTINED`, `REVOKED`, `ERROR` |
| `OFFLINE` | `RECONNECTING`, `QUARANTINED`, `REVOKED`, `ERROR` |
| `RECONNECTING` | `ENROLLMENT_PENDING`, `CONNECTED`, `OFFLINE`, `DEGRADED`, `QUARANTINED`, `REVOKED`, `ERROR` |
| `QUARANTINED` | `RECONNECTING`, `REVOKED` |
| `REVOKED` | None (terminal) |
| `ERROR` | `PROVISIONING`, `RECONNECTING`, `QUARANTINED`, `REVOKED` |

This conservative initial graph requires the normal enrollment and synchronization
sequence before `MESH`. Degraded/offline recovery goes through reconnecting.
Quarantine has no direct path to an operational state; revocation cannot be
cleared by a lifecycle transition. This is transition validation, not authorization:
future trusted event producers must separately validate their authority/evidence.

## Internal interface

`internal/localstate` defines typed constants, transition validation and errors.
The PostgreSQL store implements:

```go
Transition(ctx context.Context, expected, next localstate.State) (localstate.Snapshot, error)
```

Callers must supply the state they observed. PostgreSQL locks the singleton state
row, checks the expected state and explicit graph, then updates state and appends
a history entry in one transaction. Stale callers receive `ErrStateConflict`;
disallowed edges receive `ErrInvalidTransition`. Neither changes state, timestamp,
or history. Database errors also fail without a partially committed transition.

The read-only HTTP API uses the narrower existing repository interface. No
unauthenticated transition endpoint or configuration control is introduced.

## Storage and timestamps

Forward migration `0002` adds `node_state.changed_at` and
`node_state_transitions` with source state, target state and timestamp. The first
migration is unchanged. State values are constrained to the supported set.

A transition takes one PostgreSQL timestamp for both the current state and
history record. Timestamps are timezone-aware; history IDs preserve transaction
ordering independently of clock adjustments. Initialization/restart never changes
an existing state's timestamp. For pre-existing version-1 rows, the initial
`changed_at` records migration time—the previous transition time is unknown.
Initial provisioning and migration backfill are not invented history transitions.
An unsupported version-1 state makes migration fail instead of silently resetting it.

`GET /api/v1/status` returns the persisted state and RFC3339 `state_changed_at`
alongside the node ID, release version, and observed heartbeat connectivity.
When federation is not configured, connectivity remains `NOT_IMPLEMENTED`; an
enabled worker begins at `UNKNOWN`, then reflects successful, degraded, offline,
or reconnecting heartbeat state. Database failures still return HTTP 503 without
fabricated state. See [federation heartbeats](federation-heartbeats.md).
The complete outage and recovery path is documented in [automatic Core
reconnect](reconnect-behavior.md).

## Verification

From the repository root:

```sh
(cd node-agent && go test ./... && go test -race ./... && go vet ./...)
python3 scripts/check-node-stack.py
```

Unit tests enumerate every pair of supported states and unknown inputs. Real
PostgreSQL tests cover valid/invalid transitions, timestamps/history, stale and
concurrent callers, rollback, upgrade/replay, and persistence after reopening.
The disposable Docker test also performs an explicit transition and verifies the
same state and timestamp through Agent restart and Agent-only recreation. Its
Go probe is test tooling mounted into the build container, not an Agent CLI.

Keep the named PostgreSQL volume. Recreating an Agent container is safe; deleting
its database removes identity, state, and transition history.

Enrollment completion now requests the existing `ENROLLMENT_PENDING → CONNECTED`
transition after an authenticated approval. This is a lifecycle event, not network
detection; see [completed enrollment](node-enrollment-handshake.md).
