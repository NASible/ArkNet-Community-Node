# ArkNet Node Release Manifest

Each ArkNet Node release is one versioned, machine-readable manifest. It pins
every component of the Node stack. Operators pick a release, or later a
channel, and never choose component versions themselves.

The current manifest is [`deploy/node/release.json`](../deploy/node/release.json).
It ships beside the Compose files it describes.

## Format (`arknet.node-release/v1`)

```json
{
  "schema": "arknet.node-release/v1",
  "version": "0.0.1-dev",
  "channel": "development",
  "published_at": "2026-10-25T00:00:00Z",
  "components": {
    "arknet-agent":    { "version": "0.0.1-dev", "image": "arknet-agent:0.0.1-dev", "bundled": true },
    "arknet-ui":       { "version": "0.0.1-dev", "image": "arknet-ui:0.0.1-dev", "bundled": true },
    "arknet-transfer": { "version": "0.0.1-dev", "image": "arknet-transfer:0.0.1-dev", "bundled": true },
    "postgresql":      { "version": "18", "image": "postgres:18-alpine", "bundled": true },
    "jellyfin":        { "version": "12.2", "image": "jellyfin/jellyfin:12.2@sha256:3577…d037", "bundled": true },
    "tailscale":       { "version": "1.104.2", "image": "tailscale/tailscale:v1.104.2", "bundled": false }
  },
  "compose": {
    "project": "arknet-node-dev",
    "files": [
      { "path": "compose.yaml", "sha256": "…" },
      { "path": "compose.transfer.yaml", "sha256": "…" }
    ]
  }
}
```

The Agent's parser (`node-agent/internal/release`) is the authoritative
validator. It is strict: unknown fields, trailing data, and anything over
64 KiB are rejected.

| Field | Rule |
|---|---|
| `version` | The human-facing release version, in SemVer form `MAJOR.MINOR.PATCH` with an optional pre-release suffix. Build metadata is not allowed. |
| `channel` | `stable` or `development`. A stable version must not have a pre-release suffix, e.g. `0.1.0`. A development version must have one, e.g. `0.1.0-dev.3`. |
| `components` | Exactly these six: `arknet-agent`, `arknet-ui`, `arknet-transfer`, `postgresql`, `jellyfin`, `tailscale`. Each image must have a version tag. `latest`, `stable`, `unstable`, and untagged or digest-only references are rejected, and a digest may be added to a tag. |
| ArkNet components | `arknet-agent`, `arknet-ui` and `arknet-transfer` ship in lockstep. Their version equals the release version and they are bundled. |
| `bundled` | The component runs as a service in the release Compose files. PostgreSQL and Jellyfin must be bundled. |
| `compose` | The Compose project name plus 1–8 plain file names. Each has a SHA-256 of its exact bytes, so an upgrade tool can verify it applies exactly the definitions that were released. |

### Component pinning

- **PostgreSQL** is pinned to its major version, `18`. The data directory is
  compatible within a major, and minor/patch images carry security fixes.
- **Jellyfin** is pinned to the 12.2 multi-arch index digest that this stack
  was tested with. Jellyfin upgrades migrate its database, so they belong in an
  ArkNet release, not a floating `latest` tag. Before this prompt the Compose
  file used `jellyfin/jellyfin:latest`.
- **Tailscale** is pinned for compatibility but **not bundled yet**: no
  Tailscale service exists in the Node Compose files. The manifest records the
  supported version (stable 1.104.2) for a future installer, diagnostics, or
  host install. Bundling it later changes only this entry and the Compose files.
- `arknet-transfer` lives in `compose.transfer.yaml`, the real transfer
  overlay. `compose.transfer-mock.yaml` is a development test aid and is not
  part of the release.

## How the Agent uses it

`compose.yaml` mounts the manifest read-only at `/etc/arknet/release.json` and
sets `ARKNET_RELEASE_MANIFEST` to that path. If set, the path must be a clean
absolute path. At startup the Agent loads it and accepts it only if
`components.arknet-agent.version` equals its own build version. That way the
UI never reports a release the Node isn't actually running.

| Outcome | Log event | `GET /api/v1/status` → `release` |
|---|---|---|
| Valid and matches | `release_manifest_loaded` (with release and channel) | `{"version":"0.0.1-dev","channel":"development"}` |
| Not configured | `release_manifest_not_configured` | `null` |
| Invalid | `release_manifest_invalid` | `null` |
| Different Agent build | `release_manifest_mismatch` | `null` |

A missing or bad manifest never stops the Agent or the local services. It is
reported, not fatal.

The status `version` field remains the Agent build version. The UI dashboard
shows **ArkNet {release version}** and the **Release channel**. Without a
manifest it shows the Agent build and says the release manifest is unavailable.

## Keeping a release coherent

`node-agent/release_manifest_test.go` fails the build unless the shipped
manifest:

- parses as this Agent's current release;
- equals `VERSION` and the `node-ui` package version;
- matches every listed Compose file's SHA-256;
- lists exactly the images those Compose files run, with no `${…}` image
  overrides. Operators cannot change component versions with environment
  variables.

So any change to `compose.yaml` or `compose.transfer.yaml` needs an updated
digest:

```bash
cd deploy/node && shasum -a 256 compose.yaml compose.transfer.yaml
```

## Published releases

[Update discovery](update-discovery.md) reads a channel index of published
manifests, and [`arknet-node upgrade`](node-upgrade.md) applies them. Since
Prompt 51A a published manifest must be Ed25519-signed (`release.json.sig`) and
must pin every bundled image by digest, for example
`ghcr.io/nasible/arknet-agent:0.1.0@sha256:…`; both consumers refuse anything
else. See the [release process](release-process.md).

The checked-in `deploy/node/release.json` is the development manifest: its
ArkNet images are local `:<version>-dev` tags built from this checkout and it
is never published. PostgreSQL and Jellyfin are pinned by digest in it too.
The index itself is not signed yet.

## Verification (2026-10-25)

- `internal/release` tests: stable and development releases parse, and 26
  malformed-manifest cases are rejected, including a digest-only image. That
  case first exposed a parser bug, which is now fixed.
- Agent tests: the shipped manifest is consistent with the repository. Status
  reports the release. Invalid or mismatched manifests return `release: null`
  and keep serving. Config path validation works.
- UI: status parser cases and Dashboard stable/development/fallback rendering.
- Live: `docker compose up --build --wait` of `deploy/node/compose.yaml`
  brought all four services up healthy, including the pinned Jellyfin. The
  Agent logged `release_manifest_loaded` and the dashboard showed
  **ArkNet 0.0.1-dev** with **Release channel: Development**. The stack was
  removed with `down -v`.
