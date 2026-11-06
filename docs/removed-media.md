# Removed Media

ArkNet removal is a logical catalog transition, not remote filesystem deletion.
Core marks an ArkNet-managed object `REMOVED`; the tombstone is terminal and
retains only the object's ArkNet metadata. Core does not receive Node paths,
Jellyfin library state, or information about unrelated local files.

## Core operator action

An administrator holding a token with the `media.admin` permission (see
[Core administration](core-administration.md))
removes an active object with an empty request body:

```text
POST /api/admin/v1/media/{media_id}/remove
Authorization: Bearer <admin token>
```

The response contains the catalog object in `REMOVED` state. Repeating the
transition returns `409`; removal cannot reactivate an object. The route is in
the same protected `/api/admin/v1/` namespace as other Core administration and
is not exposed as a Node mutation.

Removal immediately makes the object ineligible for all new ArkNet activity:

- catalog selection and transfer start/resume are denied;
- Core will not issue new tracker grants;
- Core web-seed and torrent-metadata endpoints stop serving it;
- replica reports are rejected;
- `ListEligibleMediaSources` excludes it, so it is no longer advertised as a
  peer source even if a replica row and a Node's Peer Serving opt-in remain.

Replica rows may remain as ArkNet history. They contain only opaque Node/object
IDs and an availability timestamp; they do not keep a removed object eligible.

## Node transition

Nodes learn explicit `REMOVED` tombstones through signed outbound catalog
fetches. The Agent reconciles them after accepted Core heartbeats and whenever
the local media list is refreshed. Catalog omission is never treated as
removal.

For an existing selection:

- `AVAILABLE` becomes `REMOVED_RETAINED`. The verified Agent-managed media file
  remains unchanged.
- `QUEUED`, `DOWNLOADING`, and `ERROR` become `NOT_PRESENT` under the terminal
  Core `REMOVED` state because they do not represent a verified local copy.
- an active or paused transfer for a removed object is cancelled by the transfer
  reconciler. Cancel preserves staged partial bytes but they are not represented
  as an available retained copy and cannot be resumed through ArkNet.

A removed object cannot be selected, started, resumed, reported as a new
replica, advertised by Core, or served through Core's web seed. Existing
short-lived tracker grants remain cryptographically valid until expiry, but the
Agent cancels local in-flight participation and Core stops issuing replacements.
The product has no persistent seeding path: completed receiving transfers leave
the swarm. If a future seeding implementation is added, it must use the same
Core `ACTIVE` eligibility check and stop its torrent handle on removal.

## Operator-controlled local deletion

Core removal does not unlink a Node file. A Node Operator may explicitly remove
the retained managed copy in the Media UI with **Delete retained local copy** or
through:

```text
DELETE /api/v1/media/{media_id}/retained
```

The request accepts no body, query, or path. The Agent derives the sole target as
`<ARKNET_MEDIA_DIR>/<opaque-media-id>.mp4`; callers cannot supply a filesystem
path. It refuses non-regular targets, removes the file if present, transitions
the local state to `NOT_PRESENT`, and releases its logical storage reservation.
The Core tombstone remains unchanged.

The local management API has the existing trusted-local boundary. Do not expose
it to untrusted networks; browser cross-origin protection is not operator
authentication.

## Jellyfin behavior

ArkNet and Jellyfin have separate authority:

- Core removal does not call Jellyfin and does not delete the retained file.
  An item already indexed by Jellyfin can remain visible and playable from the
  local retained file according to the Node Operator's Jellyfin permissions.
- Deleting the retained copy through the Node action removes the managed file,
  but the removal action does not automatically start a Jellyfin library scan.
  Jellyfin may continue showing cached metadata until its next scheduled scan or
  until the operator starts a library scan manually. Playback will fail once the
  underlying file is gone.
- Jellyfin visibility never makes a `REMOVED` object eligible for ArkNet
  replication, advertisement, or peer serving.

## Verification

The automated checks cover the Core admin transition, terminal repository
state, denial of new transfer authorization, retention of an available local
file, cancellation of a removed in-flight transfer, explicit local deletion,
and source-selection exclusion while the replica and Peer Serving opt-in remain.

Run:

```sh
(cd core && go test -race -count=1 ./... && go vet ./...)
(cd node-agent && go test -race -count=1 ./... && go vet ./...)
(cd node-ui && npm test -- --run && npm run build)
```

Set `ARKNET_TEST_DATABASE_URL` for the real PostgreSQL repository checks.

### Live acceptance result (2026-10-25)

Compiled Core and Agent binaries ran against disposable PostgreSQL. A test
object was selected, given a verified local MP4 copy and a replica record, and
the Node opted into Peer Serving; Core listed it as an eligible source. After
`POST /api/admin/v1/media/{id}/remove`:

- Core eligible sources dropped from 1 to 0 while the replica row and the
  opt-in remained;
- the next accepted heartbeat moved the Node to `REMOVED`/`REMOVED_RETAINED`
  without any UI refresh;
- the local file stayed byte-identical (SHA-256 unchanged);
- reselection returned `409 media_removed`; repeated removal returned `409`;
- operator deletion returned `204`, removed only the managed file, set
  `NOT_PRESENT`, and left the Core tombstone unchanged;
- a dump of Core's database contained neither the Node media path nor any
  `.mp4` filename, and logs contained no admin token or database password.

Replica availability and Core media creation were seeded directly in the
disposable database for this proof; signed replica reporting and real transfer
finalization are covered by the Prompt 25/27 transfer proofs and unit tests.
