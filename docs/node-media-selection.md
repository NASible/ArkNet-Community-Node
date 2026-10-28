# Node Media Selection

Prompt 19 adds an operator-selected local queue, not an automatic cache or a
transfer worker. The Node UI's **Media** view displays the authorized ArkNet
catalog with title, byte size, local state, and **Make Available Locally**.

## Local states

- `NOT_PRESENT`: no selection on this Node.
- `QUEUED`: the operator selected the object; metadata and reservation are durable.
- `DOWNLOADING`: reserved for transfer progress; not entered in this phase.
- `AVAILABLE`: reserved for verified local availability; not entered in this phase.
- `REMOVED_RETAINED`: Core explicitly removed an object that was verified
  `AVAILABLE` locally; the managed file is kept until the Node Operator deletes
  it. Unverified selections become `NOT_PRESENT` on removal. See
  [Removed media](removed-media.md).
- `ERROR`: represented in the model/UI, but no transfer worker sets it yet.

A selection creates no media files, starts no downloads, and does not configure
Jellyfin libraries or swarm participation. Local users, arbitrary local files,
and filesystem paths are not sent to Core. Selections themselves are not sent to
Core: the Agent makes signed, outbound catalog reads only.

## Storage allocation

Set `ARKNET_MEDIA_STORAGE_ALLOCATION_GIB` to a whole GiB count in the Agent
environment or `deploy/node/.env`, then recreate the Agent:

```dotenv
# Example reservation budget of 10 GiB.
ARKNET_MEDIA_STORAGE_ALLOCATION_GIB=10
# Or use -1 instead for unlimited allocation.
```

- Positive values set a limit in GiB (1 GiB = 1,073,741,824 bytes).
- `0` (the unset default) disables new selections, including zero-size objects.
- `-1` removes the configured allocation limit; reservations are still tracked.
- Values below `-1`, fractional GiB, and values that overflow byte accounting are
  rejected at startup.

The legacy `ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES` setting remains a fallback for
existing deployments, with `-1` also meaning unlimited. Set only one allocation
variable; setting both is rejected rather than guessing units. To migrate, remove
the legacy setting and specify the desired whole GiB allocation with `_GIB`.
Internally, reservations and API byte counts remain bytes; the API uses
`allocation_bytes: -1` for unlimited, displayed as **Unlimited** by the UI.

All persisted selection sizes count against a finite allocation, including errors and
removed-retained entries. Size comes from Core, never the browser. A transaction
and shared local row lock serialize selection and reservation checks, preventing
concurrent over-allocation. Repeating a selection is idempotent: it preserves the
original size, metadata, and state without reserving twice. Lowering allocation
below existing reservations does not delete data or reset the queue; new
selections that do not fit fail.

This is a **logical reservation budget**, not a filesystem quota or free-disk
check. Unlimited disables only ArkNet's reservation limit; it does not create
free disk space. Core may initially report zero size. Future transfer work must reconcile
actual storage before writing data; no transfer behavior is implemented here.

## Local API

```text
GET  /api/v1/media
POST /api/v1/media/{media_id}/selection
```

POST requires `Content-Type: application/json` and an empty object body (`{}`).
Only an opaque media ID is accepted. The Agent resolves it from a freshly
retrieved authorized Core catalog. Missing/removed objects cannot be queued.
Cross-origin browser mutations are rejected. Responses use `Cache-Control:
no-store`; credentials, Core URLs, and database errors are never returned.

GET returns:

```json
{
  "objects": [],
  "allocation_bytes": 0,
  "reserved_bytes": 0,
  "core_available": false
}
```

Each object contains `id`, `title`, `size`, Core `state`, and `local_state`.
Successful POST returns the selected object with its durable local state.
Allocation failures return `409 storage_allocation_exceeded`; removed objects
return `409 media_removed`; unknown objects return `404 media_not_found`.
Unavailable Core returns `503 catalog_unavailable` for new selections; local
storage failures return `503 local_media_unavailable`.

The local management API follows the existing trusted-local deployment boundary:
Agent/UI ports bind to loopback in the development stack. This phase does not add
operator authentication. Do not publish the unauthenticated local management API
to untrusted networks; cross-origin protection is not operator authentication.

## Offline and removal behavior

When Core is unreachable, disabled, or enrollment is not approved, GET still
returns saved selections and reservations with `core_available: false`. New
selection fails closed; the UI does not pretend the global catalog is available.
After restart, saved selections remain visible without Core.

Only explicit Core `REMOVED` tombstones update selected objects to
`REMOVED_RETAINED`. An object absent from a bounded catalog response is **not**
inferred to be removed. The current Core catalog is limited to 100 entries;
this phase does not add catalog pagination. Retained local selections are merged
into the displayed list even when absent from that response. No files are
automatically deleted, and removed selections cannot be resurrected. Prompt 33
refines the removal transition and adds explicit operator deletion; see
[Removed media](removed-media.md).

## Verification

From repository root:

```sh
python3 scripts/check-node-compose.py
python3 scripts/check-media-selection-stack.py
```

The live check uses uniquely named disposable Core/Node Compose projects and real
PostgreSQL. It verifies signed catalog fetch, queuing, duplicate reservation
safety, allocation rejection, Agent restart/recreation, explicit tombstones, and
offline retention. It removes only its own test containers, networks, and volumes.

Within `node-agent/`, `go test -race -count=1 ./...` and `go vet ./...` check the
Agent. Set `ARKNET_TEST_DATABASE_URL` to enable isolated-schema PostgreSQL tests,
including concurrent allocation and queue persistence across pool reopening.
Within `node-ui/`, run `npm test` and `npm run build` for API validation, operator
interaction coverage, and TypeScript/production build checks.
