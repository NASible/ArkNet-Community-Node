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

## Not done (needs the operator)

- No real release is published yet. Generating the production key, committing
  `deploy/node/release-signing.pub`, `docker login ghcr.io` with
  `write:packages`, making the six packages public and hosting the release
  site are operator steps; see [release process](../release-process.md).
- The index is not signed: a hostile site can withhold updates, but cannot
  make a Node install an unsigned, modified or older release.
- There is a single signing key, with no rotation mechanism yet.
