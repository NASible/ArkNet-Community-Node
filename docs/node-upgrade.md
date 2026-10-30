# Whole-Stack Node Upgrade

`deploy/node/arknet-node` is the supported way to move a Node from one ArkNet
release to another. It runs on the host as the operator, using only Python 3's
standard library and the Docker CLI. The operator never edits Compose files,
containers, images, or volumes by hand.

```bash
sudo arknet-node status
sudo arknet-node upgrade                 # newest release on this Node's channel
sudo arknet-node upgrade --manifest URL --sha256 HEX   # one specific release
sudo arknet-node rollback                # back to the previous release
```

**Why a host command and not the Agent?** Upgrading needs the Docker socket,
which is root on the host. The Agent is a network-facing service and must not
hold that power. The Agent only *discovers* updates ([update
discovery](update-discovery.md)). Installing is an explicit operator action
until index and manifest signing exists; see Limits.

## Installation layout

`ARKNET_HOME` (default `/opt/arknet`, or `--home`) contains:

| Path | Purpose |
|---|---|
| `.env` | Operator settings and secrets. Must be mode 0600. Upgrades preserve existing settings; the first HTTPS-enabled upgrade appends `ARKNET_TLS_DIR` when absent. |
| `tls/` | Per-Node self-signed management certificate (`cert.pem`) and private key (`key.pem`, 0600). Mounted read-only into the UI; never sent to Core. |
| *(permissions)* | `ARKNET_HOME`, `releases/` and each release directory must be searchable by others (0755), and `release.json` readable (0644). The Agent container runs as UID 10001 and bind-mounts its release's `release.json`; otherwise it reports `release: null`. The tool writes releases with these modes and refuses to run if they are missing. |
| `state.json` | `arknet.node-install/v1`: installed `release`, `previous`, Compose `project`, and `compose_files` in use (e.g. only `compose.yaml`, or plus `compose.transfer.yaml`). |
| `releases/<version>/` | The verified `release.json` and Compose files of each installed release. The running release mounts its own `release.json` into the Agent. |
| `backups/` | Pre-upgrade `pg_dump -Fc` files (mode 0600). |
| `upgrade.log` | Timestamped step log. No secrets. |

The [installer](node-installation.md) creates this layout through
`arknet-node install`.

## What `upgrade` does

| Step | Checks | On failure |
|---|---|---|
| 1. Prerequisites | Docker running; Compose ≥ 2.20; ≥ 2 GiB free; `.env` present and not readable by others; `state.json` valid; installed release files unmodified (SHA-256 against their manifest) | Abort, nothing changed |
| 2. Target | Reads the channel index (`--index` or `ARKNET_RELEASE_INDEX_URL` in `.env`). Uses only this Node's channel and only strictly newer versions. "Nothing to do" when up to date. | Abort |
| 3. Download | The manifest must match the published SHA-256 and pass the strict v1 rules (the same as the Agent's parser). It must be on the same channel, newer, the **same Compose project** (otherwise volumes would be orphaned), and still provide every Compose file this Node uses. Each Compose file must match its manifest digest. HTTPS only (HTTP for loopback or `ARKNET_RELEASE_INDEX_ALLOW_HTTP=true`); no redirects. | Abort |
| 4. Images | Pulls every bundled image the manifest pins that is not already present. | Abort |
| 5. Compose | `docker compose config` on the new files with this Node's `.env`, e.g. to catch new required settings. | Abort |
| 6. Backup | `pg_dump -Fc` of the Node database, and a record of every project volume and its creation time. | Abort |
| 7. Apply | `docker compose -p <project> up -d --wait --remove-orphans --no-build --pull never` with the new release's files. Containers are replaced; **volumes are never removed**, because no `down` or `-v` is used. | Roll back |
| 8. Validate | Every pre-existing volume still exists with the same creation time. The Agent's local `status --json` command reports healthy, database healthy, Jellyfin healthy, and **`release.version` = target**, which proves it parsed the new manifest. Jellyfin `/health` returns `Healthy`. Retries for up to 120 s. | Roll back |

Only after step 8 does `state.json` record the new release, with the old one
kept as `previous`. Concurrent runs are refused by a lock.

### First upgrade with operator authentication

Refresh the host tool by rerunning the published `install.sh`, then run
`sudo arknet-node upgrade`. The upgrade creates a per-Node ECDSA P-256
certificate when missing and enables `https://<node-address>:8443`. Plain HTTP
stays on loopback. It preserves an existing certificate, repairs the private
key to mode 0600/root ownership. All UI code imported before privilege drop is
root-owned and non-writable by the runtime account; the UI clears supplementary
groups and drops root after reading the key and before opening either listener.

After validation it prints the certificate's SHA-256 fingerprint. If the Node
has no active operator, it also prints a single-use setup code valid for one
hour, directly to the terminal, not `upgrade.log`. Compare the fingerprint in
your browser, choose **Set up operator**, and enroll a password and authenticator
app. Existing operators keep their accounts and sign in normally; upgrades do
not issue another setup code while an active operator exists.

An expired or lost code can be replaced with
`sudo arknet-node operator setup-code`. Lost operator access is recovered only
from the local shell with `sudo arknet-node operator recover NAME`. Health
validation uses `arknet-agent status --json` inside the container, not the
now-operator-only HTTP status endpoint. See
[operator authentication](node-operator-authentication.md).

### Reporting

Each step is printed and logged. Exit codes:

| Exit | Meaning |
|---|---|
| `0` | Upgraded, or already up to date |
| `2` | `FAILED at <step>: <reason>` plus a hint, and **"The running Node was not changed."** |
| `3` | `UPGRADE FAILED while applying X: <reason>` followed by `ROLLED BACK: <previous> is running again and validated`, with the backup path |
| `4` | `ATTENTION`: the rollback also failed. Volumes were not removed; the output gives the backup path. |

## Recovery strategy

- **Before apply** (steps 1–6): nothing on the running stack changes, so you
  can simply retry once the reported cause is fixed.
- **Automatic rollback** (steps 7–8): the previous release's verified files and
  images are still local, so the tool re-applies and re-validates them.
- **`arknet-node rollback`**: the operator can return to `previous` after a
  successful upgrade. The tool backs up first and validates afterwards; if the
  rollback fails it re-applies the current release.
- **Database compatibility:** the Agent applies only its own migrations and
  ignores newer ledger rows. Rolling back the *containers* therefore works
  while migrations stay additive, which is ArkNet's migration convention.
  If a release ever needs a non-additive migration, its release notes must say
  rollback requires restoring the database.

### Manual recovery

Use this after exit `4`, or to undo a migration. Run it from `ARKNET_HOME`,
with `R` set to a release directory under `releases/`:

```bash
set -a; . ./.env; set +a
docker compose -p <project> --env-file .env -f releases/$R/compose.yaml stop arknet-agent
docker compose -p <project> --env-file .env -f releases/$R/compose.yaml exec -T postgres \
  sh -c 'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists' < backups/<file>.dump
docker compose -p <project> --env-file .env -f releases/$R/compose.yaml up -d --wait
```

Then set `release` in `state.json` to `R`. Never use `down -v`; it deletes the
Node's data.

## Publishing a release

A published release is a directory containing `release.json` and every
Compose file it lists, plus the channel index pointing at it (see [update
discovery](update-discovery.md)). Release Compose files must:

- use registry image references matching the manifest;
- contain no `build:` sections;
- keep the same `name:` / project as earlier releases;
- add new settings with defaults, so existing `.env` files still validate.

`scripts/check-node-upgrade-stack.py` shows the transformation from the
development Compose files.

Image tags are treated as immutable per release: a tag that is already present
locally is not pulled again. Pin third-party images by digest, as Jellyfin
already is. The Agent build version can be stamped with
`docker build --build-arg ARKNET_VERSION=<release>`; the default remains
`VERSION`.

## Limits

- **No signing yet:** index and manifests are not signed. The tool verifies
  digests from the index over HTTPS. Signing should exist before upgrades are
  triggered automatically or the default index is public.
- **Operator-triggered only:** the Agent's "automatic updates" setting is
  recorded but not acted on yet.
- **The tool doesn't upgrade itself:** `arknet-node` is not updated by the
  upgrade; the installer will ship it.
- **Tailscale is untouched:** it is host-provided and not bundled.

## Verification

- `deploy/node/arknet_node_test.py`: 26 unit tests with a fake Docker CLI and
  a loopback release host. They cover:
  - the success-path order (pull → backup → apply, with `--no-build --pull
    never` and no `down`/`-v`);
  - up-to-date no-op;
  - aborts for digest mismatch, tampered Compose file, unpullable image, bad
    config, backup failure, project/channel change, downgrade, modified
    installed files, and world-readable `.env`;
  - rollback after a failed apply or unhealthy Jellyfin;
  - recreated-volume detection;
  - failed rollback;
  - manual rollback;
  - locking;
  - validator parity with the Agent and SemVer precedence;
  - an Agent still reporting the old release;
  - same-version reinstall;
  - an unreadable mounted manifest.
- Ten mutations of the tool were each caught by the unit tests: skipped
  manifest or Compose digest, skipped running-release validation, skipped
  volume check, no automatic rollback, pulling during apply, same-version
  reinstall, no backup, project change, unreadable-manifest check.
- `scripts/check-node-upgrade-stack.py`, live on real Docker (2026-10-25):
  1. It builds Agent images stamped `0.0.2-dev.1` and `0.0.2-dev.2` and pushes
     them to a throwaway local registry, keeping no local copies. It publishes
     the releases from a loopback release host and installs `0.0.2-dev.1`.
  2. It seeds data: automatic updates off, media-miss policy MANUAL, and a
     marker file in Jellyfin `/config`.
  3. **Acceptance:** `arknet-node upgrade` alone moved the Node to
     `0.0.2-dev.2` in about 15 s. It pulled the three ArkNet images from the
     registry and kept the pinned PostgreSQL and Jellyfin images. Afterwards
     the Agent reported release `0.0.2-dev.2`, with Node ID, settings and the
     Jellyfin marker unchanged, every volume keeping its original creation
     time, Jellyfin healthy, and a 0600 backup written.
  4. Re-running was a no-op.
  5. An unpullable release stopped at `images` with exit 2, and the Node was
     unchanged.
  6. A release whose Agent exits after apply produced `UPGRADE FAILED while
     applying …: container … exited (1)`, then an automatic, validated
     `ROLLED BACK` to `0.0.2-dev.2` (exit 3), with data preserved.
  7. `arknet-node rollback` returned to `0.0.2-dev.1`, with data preserved.
  8. No secrets appeared in `upgrade.log`, and everything was cleaned up.
- An earlier live attempt found a real installation hazard. If the bind-mounted
  `release.json` is unreadable to the Agent container, the Node silently runs
  with `release: null`. The tool now writes release files 0644 in 0755
  directories and refuses to proceed while they are not readable.
