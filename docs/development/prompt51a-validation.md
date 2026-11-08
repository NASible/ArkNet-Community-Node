# Prompt 51A validation — Versioned, signed releases from a registry

Branch `phase21/51a-signed-releases`. Runs on 2026-11-07 AKST. Logs are local
evidence under `~/.hermes/cache/scratch/p51a/` and are not committed.

## Decisions (D11, D12)

- **Registry (D11):** GitHub Container Registry, public packages, anonymous
  pulls. Images are `ghcr.io/nasible/arknet-<component>:<version>` and are
  always referenced as `…@sha256:<digest>`. Builds target `linux/amd64` and
  `linux/arm64`.
- **Signing (D12):** one offline Ed25519 key. The signature is a detached
  base64 `release.json.sig` over `arknet.node-release-signature/v1\n` plus the
  exact `release.json` bytes. The Agent verifies with Go `crypto/ed25519`;
  `arknet-node` verifies with OpenSSL 3 (`pkeyutl -rawin`). The public key
  ships in `install.sh`, which writes it to `.env` as
  `ARKNET_RELEASE_VERIFY_KEY`.
- **Tests:** throwaway local registries and per-run throwaway keys. The real
  registry and key are used only by `scripts/publish-release.py`.

## Acceptance criteria

| Criterion | Result | Evidence |
| --- | --- | --- |
| Unit tests: tampered manifest, wrong key and unsigned manifest are refused; a valid one is accepted | **PASS** | See below. |
| `check-node-upgrade-stack.py` upgrades A → B from signed, digest-pinned manifests and rejects a tampered B with no change | **PASS** | See below. |
| `check-node-install-vm.py` installs from the registry on a fresh VM | **PASS** | See below. |
| No Compose file references an unpinned image (enforced by a test) | **PASS** | See below. |

**Unit tests.**

- **`arknet-node`:** `deploy/node/arknet_node_test.py` has 10 new tests. They
  cover:
  - an unsigned release, a release signed by another key, and a tampered
    manifest whose index digest matches;
  - six malformed signature forms;
  - a missing or non-canonical key, and an unpinned image;
  - a fresh install refusing an unsigned release;
  - the OpenSSL vectors shared with the Agent, and key parsing.

  Each refusal is checked to leave no staged release and to pull no image.
  The tests failed before the change (14 failures, 2 errors) and pass after
  (47 OK).
- **Agent:** new tests in `node-agent/internal/release` and
  `internal/updates`, plus config tests. `go test ./...` passes in all
  packages.

**Upgrade check.** `node-upgrade.log` `ALL PASS`, 23 checks:

- Discovery reports the tampered B as `CHECK_FAILED`.
- The tampered B is refused with exit 2 ("Release signature rejected").
- The unsigned B is refused with exit 2 ("Release signature unavailable: HTTP 404").
- In both refusals there is no `releases/B` directory, the Agent still runs A,
  and no B image is present.
- The genuine B is discovered, its signature is verified, it is pulled by
  digest and installed.
- Existing cases (unpullable release, automatic rollback, manual rollback,
  no secrets) still pass.

**Fresh-VM install.** `install-vm.log` `ALL PASS` on a fresh Ubuntu 24.04 VM.
The installer:

- printed `signature verified (release key SHA256:…)`;
- wrote the shipped key to `.env` and kept `release.json.sig`;
- runs the Agent and UI from `localhost:15041/…@sha256:` references and
  PostgreSQL and Jellyfin by digest;
- reached ENROLLMENT_PENDING, and a re-run was safe.

**Compose pinning.** `scripts/release_site_test.py`
`test_every_compose_image_is_pinned_or_built_from_source` checks every tracked
Compose file:

- third-party images need a digest;
- ArkNet images need a `build:` from this checkout or a digest.

Unpinning PostgreSQL makes it fail. Generated release Compose files are
digest-only (generator test). PostgreSQL is pinned to
`postgres:18-alpine@sha256:123b1799…` in `deploy/core/compose.yaml`,
`deploy/node/compose.yaml` and `deploy/node/release.json`.

## Release script

`scripts/publish-release.py publish` was run end to end by
`scripts/publish_release_test.py` (`publish.log`, OK in 101 s). It used a
temporary clone holding the working tree, a throwaway annotated tag
`v0.0.9-rc.1` and key, and a local registry. The run:

- exported the tag with `git archive`;
- built and pushed all six images with BuildKit and recorded six digests in
  `images.json`;
- confirmed each manifest digest in the registry;
- signed the release and verified the site the way `arknet-node` does;
- produced pulled images labelled with the version and commit;
- refused a second publish of the same version before building anything, and
  left no buildx builder behind.

The test ran for the native platform only; the real default is
`linux/amd64,linux/arm64`.

## Mutation checks

- **`arknet-node`:** each of these mutants was caught by the unit tests:
  - skipping signature verification;
  - skipping the digest-pin rule;
  - accepting a failed OpenSSL verification;
  - signing without the domain prefix;
  - tolerating a missing key.

  The missing-key mutant first survived; the test was tightened to require the
  specific refusal before any download.
- **Agent:** skipping `ed25519.Verify` and skipping `RequirePinnedImages`
  were both caught.

## Defect found and fixed during validation

The first fresh-VM run failed: `docker tag arknet-transfer:0.0.1-dev` found no
image. The new upgrade-check cleanup had run `docker rmi -f` by image ID,
which also deleted the shared local `arknet-transfer:0.0.1-dev` tag. Cleanup
now removes only test-registry references. The rerun passed, and the local tag
survived both checks.

## Live pilot: GHCR release on a fresh Proxmox VM (2026-11-07)

**Regression first.** `main` at `188daac` was redeployed to the four pilot VMs.
All checks passed: deployment and access boundaries, three-Node media proof,
and backup decrypt-and-restore on every VM. No secrets appeared in the logs.

**Test machine.** `nodenet-node-d` (VMID 112, host `pve`, LAN only; see
`deploy/live-test/README.md`) is a fresh Ubuntu 24.04 VM with no Docker.

**Test releases.** `0.1.0-pilot.1` and `0.1.0-pilot.2` were published to
the development channel with `scripts/publish-release.py`. They were built
from temporary clones of `main` with a throwaway signing key, for
`linux/amd64`, and pushed to the public `ghcr.io/nasible/arknet-*` packages.
The release site was served over plain LAN HTTP from the operator Mac
(`--allow-insecure-http`); the signature check still ran.

| Step | Result |
| --- | --- |
| `curl …/install.sh && sudo sh install.sh --channel development --allow-insecure-http` | Installed Docker, printed `signature verified`, pulled all five images by digest from GHCR and Docker Hub, all containers healthy. The Node stays in PROVISIONING because no Core was given (see below). |
| Agent discovery on pilot.1 | `UP_TO_DATE`; after pilot.2 was published, `UPDATE_AVAILABLE` |
| Tampered pilot.2 (Agent image digest changed, index digest recomputed so only the signature differs) | Agent `CHECK_FAILED`. `arknet-node upgrade` exit 2: "Release signature rejected … The running Node was not changed." Status, releases directory and running images were identical before and after. |
| Unsigned pilot.2 (`.sig` removed) | `arknet-node upgrade` exit 2: "Release signature unavailable: HTTP 404". No change. The Agent check was rate-limited, so its result was the cached one from the tampered case. |
| Genuine `arknet-node upgrade` pilot.1 → pilot.2 | UPGRADED. The Node ID and operator session survived; PostgreSQL and Jellyfin kept running. |
| `arknet-node rollback` | ROLLED BACK to pilot.1, validated, database backup written. Same Node ID. |

node-d was left on pilot.2.

**Defects found and fixed.**

- **Go crashed under emulation.** Building amd64 images on the arm64 Mac
  crashed the Go toolchain running under QEMU
  (`fatal error: found pointer to free object`). All build stages now run on
  `$BUILDPLATFORM` and cross-compile; a test enforces it.
- **Private packages crashed the publish script.** GHCR refuses anonymous
  tokens for private packages with 401, which escaped as a traceback. It is
  now reported as "not public", and `--wait-public` waits while the operator
  changes visibility. New GHCR packages start private, and visibility can only
  be changed in the web UI.

**Gap found (not fixed).** An installed Node cannot trust a private CA, so it
cannot enroll with the pilot Core (`core.nodenet.arknet.media` uses the
pilot's private CA). Private deployments need a supported way to add a CA
bundle to the Agent.

## Not done (needs the operator)

- No real release is published yet. Generating the production key, committing
  `deploy/node/release-signing.pub`, `docker login ghcr.io` with
  `write:packages`, making the six packages public and hosting the release
  site are operator steps; see [release process](../release-process.md).
- The index is not signed: a hostile site can withhold updates, but cannot
  make a Node install an unsigned, modified or older release.
- There is a single signing key, with no rotation mechanism yet.
