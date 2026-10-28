# Core-to-Node media transfer

Prompt 25 connects an operator's **Make Available Locally** action to a real,
resumable private BitTorrent v2 transfer, atomic local import, Jellyfin refresh,
and an authenticated replica report to Core.

## End-to-end flow

1. The browser asks the loopback Node Agent to select an active catalog object.
   The browser never receives swarm URLs, grants, the transfer-service token, or
   the Node enrollment key.
2. The Agent persists the storage reservation and requests a fresh, object-scoped
   `download` grant from Core with its Ed25519 enrollment identity.
3. Core verifies that the Node is approved/trusted and that the object is active,
   then returns short-lived tracker and optional web-seed URLs for that object's
   full v2 info hash.
4. The Agent sends the opaque object ID, exact byte size and v2 hash, plus those
   URLs, over the private authenticated Agent-to-transfer control channel.
5. Core returns separate short-lived URLs for the tracker, web seed, and exact
   private v2 torrent metadata. The transfer service validates the metadata's full
   hash and total size before adding it to libtorrent. DHT, PEX and LSD remain
   disabled. This explicit metadata path is required because libtorrent does not
   exchange metadata for private magnet-only torrents.
6. Transfer state and sanitized libtorrent fast-resume data are persisted in the
   transfer-state volume. Short-lived bearer URLs are memory-only and are removed
   from manifests and resume data.
7. After interruption, the Agent obtains fresh grants and refreshes the existing
   transfer before resuming. Already verified pieces are retained; a restart must
   not silently reset progress to zero. A resumed handle always receives the
   validated Core metadata, because resume data carries none and a web seed
   cannot supply it. On every refresh the worker replaces the tracker URL, but it
   keeps the live web-seed URL for up to 120 s before switching to the fresh one.
   Removing it would abort libtorrent's in-flight range request and discard its
   data. 120 s is well inside the 5-minute grant lifetime.
8. Only a real `mock:false` completion with validated metadata is finalized. The
   Agent atomically moves the verified opaque-ID file from transfer staging into
   ArkNet media storage, marks the local selection `AVAILABLE`, requests a Jellyfin
   library refresh, and signs an idempotent replica report to Core.
9. Core accepts the replica only when Node, object, size, hash, state and trust all
   still match. Core stores the Node/object association and availability time; it
   never stores the Node's local path.

A mock transfer completion is never availability. A failed verification must leave
no final media file, set the local selection to `ERROR`, skip Jellyfin refresh, and
skip the Core replica report.

## Deployment

Use the normal Core stack with both authorization-gated source overlays and the
Node stack with the real transfer overlay. The examples assume DNS names and TLS
that are reachable from the Node; plaintext settings are for isolated local
Docker development only.

### Core and tracker

Generate a dedicated Ed25519 tracker-grant keypair. Do not reuse enrollment, TLS,
administration or user keys. The private key is mounted only into Core and the
public key only into the tracker. See [Private ArkNet tracker](private-tracker.md)
for canonical file encoding and rotation.

```dotenv
# deploy/core/.env (illustrative production origins)
ARKNET_TRACKER_ANNOUNCE_URL=https://tracker.example.net
ARKNET_TRACKER_SIGNING_KEY_FILE=/run/secrets/arknet-tracker-signing-key
ARKNET_TRACKER_VERIFY_KEY_FILE=/run/secrets/arknet-tracker-verify-key
ARKNET_CORE_WEB_SEED_ORIGIN=https://core.example.net
ARKNET_CORE_WEB_SEED_SOURCE=/srv/operator-approved/arknet-web-seed
```

The host web-seed source contains one regular file per approved object, named by
its exact opaque ID:

```text
media_<64 lowercase hexadecimal characters>
```

The catalog byte size and v2 info hash must match that file. Mount only this
operator-approved directory read-only, not a host media library, home directory,
or filesystem root.

```sh
cd deploy/core
docker compose \
  -f compose.yaml \
  -f compose.tracker.yaml \
  -f compose.web-seed.yaml \
  up --build -d --wait
```

Terminate tracker and web-seed traffic with verified TLS. Configure proxies not
to log request paths: both `/announce/<grant>` and the final web-seed path segment
contain bearer credentials.

### Node, transfer service and Jellyfin

Generate a distinct local transfer-control token and store it in the Node secret
manager or deployment `.env`; do not commit it:

```sh
export ARKNET_TRANSFER_TOKEN="$(openssl rand -hex 32)"
cd deploy/node
docker compose -f compose.yaml -f compose.transfer.yaml up --build -d --wait
```

The real overlay supplies the transfer service with:

- a private control network shared only with the Agent;
- a writable staging volume and a separate writable resume-state volume;
- the trusted tracker origin and optional trusted Core web-seed origin;
- a rate limit when an operator deliberately configures one;
- no Core database, Node database, enrollment key, admin token, browser port, or
  arbitrary host path.

The final ArkNet media volume is writable by the Agent/finalizer and mounted
read-only in Jellyfin. Keep staging, state and final media on the same filesystem
when possible so the final rename is atomic. Ownership must permit the non-root
transfer and Agent container users to write only their assigned directories.
Never grant Jellyfin write access to transfer state or Node/PostgreSQL data.

Configure Jellyfin once, create an ArkNet Movies library rooted at
`/srv/arknet/media`, and give the Agent a dedicated Jellyfin API key through the
Node secret mechanism. The key needs only the ability required to trigger a
library refresh; it is not a Core credential and must not be exposed to the UI.
Jellyfin remains independently usable when Core is offline.

## Operator procedure

1. Confirm Core, private tracker, Node Agent, real transfer service and Jellyfin
   are healthy. Confirm the Node is enrolled, approved and connected.
2. Confirm the catalog object is `ACTIVE`, has a lowercase 64-character
   `info_hash_v2`, and its declared size fits the Node allocation.
3. Confirm Core has the matching `<media_id>.torrent` metadata file and media
   object in its web-seed root. An authorized peer is optional fallback capacity;
   Core's authenticated metadata and web-seed endpoints can bootstrap the transfer.
4. In the Node Media page select **Make Available Locally** once. This one action
   persists the reservation and starts the authorized transfer.
5. Watch byte progress. `DOWNLOADING` is not local availability. After verified
   completion the media row becomes `AVAILABLE`; Jellyfin should discover the
   item, and Core should contain one replica row for this Node/object pair.

Do not copy grant URLs into `curl`, browser tools, tickets or logs while debugging.
Inspect state names, byte counts and sanitized event names instead.

## Interruption and recovery

A source outage, process restart or expired grant is recoverable:

1. Leave the transfer staging and state volumes intact.
2. Restore connectivity to Core/tracker/web seed or an authorized seed.
3. Use **Resume** if the UI presents a paused transfer. The Agent revalidates the
   active catalog object and requests fresh object-scoped URLs before resuming.
4. Confirm completed bytes continue from retained verified pieces rather than
   returning to zero. Repeated start/resume requests must not create a second
   transfer or duplicate the final file.

Do **not** delete `.fastresume`, staging, or transfer-state data as a first response.
Do not manually move a partial file into the media library. If verification fails,
fix the source/catalog mismatch and explicitly retry through the operator path.

## Verification

The repository includes a destructive-to-itself, non-destructive-to-everything-
else acceptance check:

```sh
python3 scripts/check-core-to-node-transfer-stack.py
```

It creates a uniquely named disposable environment and:

- generates a playable, multi-piece MP4 with `ffmpeg` and probes it;
- creates a private v2-only torrent and a dedicated signing keypair;
- runs Core/PostgreSQL, private tracker/web seed, an authorized seed peer, Node
  Agent/PostgreSQL, real transfer service and Jellyfin;
- enrolls and approves the Node, configures Jellyfin, then invokes the same Agent
  selection/start path used by the operator UI;
- observes nonzero partial progress, stops the seed and Core source, restarts the
  transfer service, proves sanitized fast-resume state retained progress, restores
  the source, and waits for `AVAILABLE`;
- streams the imported bytes through Jellyfin and verifies the source digest;
- reads back the exact `media_replicas` Node/object row from Core PostgreSQL;
- scans API responses, persisted transfer state and container logs for known
  credentials/authorization URLs and verifies exact project cleanup.

The check does not use a developer `.env`, existing project, existing volume or
fixed host port. Its `finally` block removes only resources carrying its unique
project/image names and verifies that each is gone.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Transfer stays at zero with a healthy web seed | Confirm Core serves the authorized `torrent_url`, and that its private v2 metadata exactly matches catalog hash and size. |
| Grant request fails | Node must be approved/trusted; object must be active with a valid full v2 hash; Node and Core clocks must agree. |
| Tracker rejects downloader or seeder | Downloader needs `download`; a peer announcing `left=0` needs `seed`. Check keypair match and grant expiry without printing the URL. |
| Progress survives pause but not container recreation | Verify both staging and resume-state volumes are persistent and distinct; do not persist bearer URLs. |
| Completion remains `DOWNLOADING` or becomes `ERROR` | Compare exact catalog size/hash with private-v2 metadata and source bytes. Never force `AVAILABLE`. |
| File is available but absent in Jellyfin | Confirm the final file is under the configured read-only Jellyfin library root, permissions allow reads, and the dedicated refresh call succeeded. A normal Jellyfin scan can be requested safely. |
| Core has no replica row | Check Agent logs for sanitized replica-report failure, Node trust, object state, exact size/hash, and Core reachability. Reporting is idempotent and may be retried. |
| Resume fails after a long outage | Grants are deliberately short-lived. Resume through Agent so it fetches fresh per-transfer tracker and web-seed URLs; never reuse a captured old URL. |

## Security invariants

- Opaque object IDs, not operator paths, cross service boundaries.
- The browser receives no transfer token, tracker/web-seed URL, grant, Jellyfin API
  key, enrollment private key, database credential, or local filesystem path.
- Transfer receives object-scoped short-lived source URLs, not Core admin or Node
  enrollment credentials.
- Tracker has only the dedicated public verification key and no database access.
- Core web-seed root and Jellyfin media root are narrowly mounted and read-only to
  readers.
- DHT, PEX, LSD, UPnP and NAT-PMP remain disabled for private transfers.
- Successful byte movement is insufficient: metadata validation, final placement,
  local `AVAILABLE`, Jellyfin refresh and the signed Core replica row are separate
  required proofs.
