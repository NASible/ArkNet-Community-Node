# Media-Miss Events

A media miss happens when [media routing](media-routing.md) sends a user to
Core because their home node lacks the object. The home Node's operator can
then make it available locally, ignore it, or let automatic mode fetch it.

## When a miss is recorded (Core)

`GET /api/routing/v1/media/{id}` records a miss only when all of these hold:

- the caller has a home node;
- the object is `ACTIVE`;
- the home node has no verified replica of it.

These cases are not misses:

- an offline home node that does have the object;
- a user with no home node;
- unknown or removed objects.

The write also re-checks those conditions, so a stale miss can't be recorded.

### Core fallback is never delayed

The miss is recorded **after the routing response is written** and never awaited:

- each record runs in its own goroutine with a 3 s timeout;
- at most 16 records run at once, and further events are dropped, not queued;
- a slow, locked, or failing database can't delay or change the fallback. A
  stalled write is abandoned when it times out.

Misses are advisory counters, so losing one under database pressure is
acceptable. Delaying playback is not.

### Privacy

`media_miss_events` is aggregated per (Node, object). It holds `miss_count`,
`first_missed_at` and `last_missed_at`, and has **no user or membership column**.

A Node reads only its own misses, outbound and signed:

```text
GET /api/federation/v1/nodes/{node_id}/media-misses
→ {"misses":[{"media_id":"media_…","miss_count":3}]}
```

The response has no user, member ID, timestamps, or other Node data. It lists
only objects that are still `ACTIVE` and that the Node still lacks, up to 100,
most-missed first. A verified replica clears the miss from the listing.
Limitation: a Node with very few members can infer that one of them requested
an object. That is inherent to notifying a home Node at all.

## Node policy (Agent)

There is a Node-local setting, default **`ASK`**:

```text
GET /api/v1/settings/media-miss-policy
PUT /api/v1/settings/media-miss-policy   {"policy":"MANUAL"|"ASK"|"AUTOMATIC"}
```

The Agent syncs misses outbound on the heartbeat interval. It keeps only misses
for objects that are `ACTIVE` and transferable in a fresh Core catalog. Title
and size come from that catalog.

- **`MANUAL`**: nothing is surfaced; the operator curates media directly.
- **`ASK`**: the operator is notified. The Media page shows **Missed on this
  Node** with **Make Available Locally** and **Ignore**.
  - Make Available Locally uses the normal selection path:
    `POST /api/v1/media/{id}/selection`. That path enforces the allocation and
    catalog checks and auto-starts the transfer like any other selection.
  - Ignore uses `POST /api/v1/media-misses/{id}/ignore` (body `{}`). It is
    sticky: new misses for that object don't resurface it.
  - A notification disappears once the object has any local selection.
- **`AUTOMATIC`**: queues and starts at most one missed object per sync. It
  obeys:
  - **storage quota**: the normal allocation-checked `QueueMediaSelection`.
    Objects too large stay as notifications, and smaller ones are tried.
  - **object availability**: only `ACTIVE` catalog objects. It starts through
    the same `authorizeStart` path as the operator, which needs a fresh Core
    catalog and transfer grant.
  - **bandwidth policy**: nothing starts while any real transfer is
    downloading or paused, so there is one transfer at a time. Transfers run at
    the transfer engine's configured `ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT`.
  - Without a configured transfer engine it does nothing, rather than
    reserving storage that will never be filled.

`GET /api/v1/media-misses` returns `{"policy","notifications":[…]}`. In
`MANUAL` the notification list is empty. Notifications are stored locally in
`media_miss_notifications` and contain no user data.

## Verification

- Core:
  - `media_misses_test.go`: fallback returns within 2 s while the recorder is
    blocked; misses only when the home node lacks the object; recorder errors
    are ignored; saturation drops without blocking; Node-scoped signed listing.
  - `internal/postgres/media_misses_test.go`: real PostgreSQL aggregation,
    held/removed/blocked handling, and no user columns.
- Agent:
  - `media_miss_handlers_test.go`: ASK queue and ignore, sticky ignore, MANUAL,
    strict input, AUTOMATIC quota/availability/one-transfer rules, no
    reservation without an engine, Core unavailable.
  - `internal/postgres/mediamiss_test.go`: default policy, persistence, schema
    constraint, ignore persistence.
  - `internal/catalog/misses_test.go`: strict signed client.
- UI: `Media.misses.test.tsx` (notify, queue from notification, ignore, MANUAL,
  policy setting) and `api/mediaMisses.test.ts`.
- Mutation checks, each caught: recording despite a held replica, synchronous
  recording, counting an offline home that has the media, removing the
  one-transfer rule, removing the availability filter, resurrecting ignored
  notifications. A behavior-neutral control mutation correctly went uncaught.
- Live run (2026-10-25), with real Core, a real Agent, and PostgreSQL:
  - with `media_miss_events` locked `ACCESS EXCLUSIVE`, the Core fallback
    returned in 3 ms (baseline 3 ms) while the miss insert was visibly blocked;
  - the stalled write was dropped after its 3 s bound;
  - later misses were counted;
  - the ASK notification reached the operator with catalog title and count and
    no user identity;
  - queuing from it produced `QUEUED`;
  - Ignore was sticky across new misses;
  - AUTOMATIC with no transfer engine reserved nothing;
  - neither database held user identity from misses.
