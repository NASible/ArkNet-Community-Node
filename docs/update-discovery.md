# Update Discovery

A Node discovers newer ArkNet releases on its own channel and shows them to
the operator. Discovery is **read-only**. It never downloads images, edits
Compose, or restarts anything. Installing is the operator's
`arknet-node upgrade` ([Node upgrade](node-upgrade.md)); it will be automatic
only once releases are signed and discovery has proven reliable.

## Channel index (`arknet.node-release-index/v1`)

ArkNet publishes one small JSON index listing the newest release on each
channel:

```json
{
  "schema": "arknet.node-release-index/v1",
  "channels": {
    "stable":      { "version": "0.1.0",     "manifest_url": "0.1.0.json",     "manifest_sha256": "…" },
    "development": { "version": "0.1.1-dev", "manifest_url": "0.1.1-dev.json", "manifest_sha256": "…" }
  }
}
```

- Channels are `stable` and `development`. The version must suit the channel,
  following the same rule as the [release manifest](release-manifest.md).
- A channel may be absent, which means nothing is published on it.
- `manifest_url` may be relative to the index URL.
- Unknown fields, trailing data, and responses over 64 KiB are rejected.

## How a check works (`node-agent/internal/updates`)

1. The Agent reads its running release and channel from its current manifest.
2. It fetches the index. HTTPS is required; HTTP is accepted only for loopback
   or with `ARKNET_RELEASE_INDEX_ALLOW_HTTP=true`, which is for test indexes.
   Redirects are not followed, so a redirect can't downgrade the connection to
   HTTP. The timeout is 10 s.
3. It takes only the entry for its own channel, so a development Node never
   offers a stable release and vice versa. If that version is not strictly
   newer by SemVer 2.0 precedence, the result is `UP_TO_DATE`.
4. Otherwise it fetches the manifest and accepts it only if all of these hold:
   - its SHA-256 equals `manifest_sha256`;
   - the strict manifest parser accepts it;
   - its version and channel match the index entry.

   Only then does the state become `UPDATE_AVAILABLE`. Anything else is
   `CHECK_FAILED`, so an unverifiable release is never shown as available.

The Agent checks at startup and every 6 hours. Operators can also use
**Check for updates**, which is coalesced to one fetch per 10 s. A transient
failure keeps showing a release that was already verified, with state
`CHECK_FAILED`.

| State | Meaning |
|---|---|
| `NOT_CONFIGURED` | `ARKNET_RELEASE_INDEX_URL` is unset. Nothing is fetched. |
| `NO_CURRENT_RELEASE` | There is no valid manifest for this Agent build, so there is nothing to compare. Nothing is fetched. |
| `NOT_CHECKED` | Configured, but no check has finished yet. |
| `UP_TO_DATE` | Nothing newer is published on this channel. |
| `UPDATE_AVAILABLE` | A newer, verified release exists. |
| `CHECK_FAILED` | The index was unreachable or invalid, or the manifest was rejected. |

**Privacy:** requests carry no Node ID, credentials, cookies, or query
parameters. The User-Agent is the fixed string `arknet-agent`. The index host
still sees the Node's IP address. Failures are logged as fixed categories
(`index_unavailable`, `index_invalid`, `manifest_rejected`); URLs and response
content are never logged.

**Limitation:** the index and manifests are not signed yet. A digest in the
index proves only that the manifest matches the index; HTTPS protects the
index itself. Signing is required before Prompt 39 installs anything
automatically.

## Automatic updates setting

| API | Body |
|---|---|
| `GET /api/v1/settings/automatic-updates` | → `{"enabled":true}` |
| `PUT /api/v1/settings/automatic-updates` | `{"enabled":false}` exactly; same-origin only |

- The default is **ON** (`COALESCE(…, true)`) for fresh and upgraded Nodes.
- The setting is stored in `node_configuration.settings.automatic_updates`. A
  migration (`0010`) constrains it to a JSON boolean.
- Opting out survives Agent restarts and full stack recreation.

Today the setting records consent only. The API reports
`automatic_install_supported: false`, and the UI says plainly that this
version discovers updates but does not install them.

## Operator API and UI

`GET /api/v1/updates` returns:

```json
{"current_version":"0.0.1-dev","channel":"development",
 "available":{"version":"0.0.2-dev","channel":"development","published_at":"2026-10-26T00:00:00Z"},
 "automatic_updates":true,"automatic_install_supported":false,
 "discovery":{"state":"UPDATE_AVAILABLE","checked_at":"2026-10-25T12:12:55Z"}}
```

`POST /api/v1/updates/check` takes `{}` exactly and is same-origin only. It
returns the same shape. A failed check is reported in the body, not as an HTTP
error.

Settings → **ArkNet updates** shows the current version, available version,
update channel, automatic updates status and last check time. It also has the
automatic-updates switch and **Check for updates**.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `ARKNET_RELEASE_INDEX_URL` | Unset (discovery off) | Channel index URL. |
| `ARKNET_RELEASE_INDEX_ALLOW_HTTP` | `false` | Allows a non-loopback HTTP index (testing only). |

No official ArkNet index exists yet, so the Compose default is empty. When one
is published, its URL becomes the release default; operators should not need
to set it.

## Verification (2026-10-25)

- Unit and handler tests:
  - SemVer precedence over a 21-version ordered table.
  - Channel isolation.
  - Same, older, or unpublished versions report `UP_TO_DATE`.
  - 12 unverifiable publications are rejected: digest mismatch, version or
    channel mismatch, invalid or missing manifest, HTTP elsewhere, schema
    errors, oversized index.
  - No redirects followed; no identity sent.
  - Coalescing; a verified release is retained through failures.
  - API strictness and same-origin enforcement.
  - Real PostgreSQL: default ON, opt-out persists across re-initialization,
    non-boolean values rejected by the schema.
- UI: panel fields, check, toggle off, unconfigured / no-release explanations,
  failure text, API parsing.
- Eight mutations were each caught: same version treated as an update, digest
  check skipped, wrong channel, trusting the index over the manifest,
  following redirects, pre-release precedence, default OFF, opt-out not saved.
  A control mutation exposed an untested query-string check, which now has a
  test and returns 400.
- Live, on the real Node Compose stack plus a throwaway index container
  publishing `0.0.2-dev`:
  - startup discovery reported `UPDATE_AVAILABLE 0.0.2-dev` with automatic
    updates ON;
  - the index saw only the index and manifest paths, with no Node ID;
  - no images changed;
  - after the operator disabled automatic updates, the setting stayed `false`
    across `docker compose restart arknet-agent` and a full `down`/`up`;
  - the Settings panel showed all four fields.
