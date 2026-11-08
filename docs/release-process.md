# Release process

ArkNet releases are built from an annotated git tag, pushed to the GitHub
Container Registry and described by a signed, digest-pinned Node release
manifest (Prompt 51A, decisions D11 and D12).

| Item | Value |
| --- | --- |
| Registry | `ghcr.io/nasible`, public packages, anonymous pulls |
| Images | `ghcr.io/nasible/arknet-{core,tracker,relay,agent,transfer,ui}:<version>`, always deployed as `…:<version>@sha256:<digest>` |
| Platforms | `linux/amd64` and `linux/arm64` |
| Signing | One offline Ed25519 key. The public key is `deploy/node/release-signing.pub`; the private key never enters the repository |
| Verification | The Agent (Go `crypto/ed25519`) for update discovery; `arknet-node` (OpenSSL 3) before installing or upgrading |
| Release site | Static files written by `scripts/publish-release.py`, for example on GitHub Pages |

## Signature format

- The signed message is the ASCII line `arknet.node-release-signature/v1`
  plus a newline, followed by the exact bytes of `release.json`.
- `release.json.sig` is the standard padded base64 of the 64-byte Ed25519
  signature, with at most one trailing newline.
- `ARKNET_RELEASE_VERIFY_KEY` and `release-signing.pub` hold the canonical
  padded base64 of the raw 32-byte public key.
- Every bundled image in a published manifest must end in
  `@sha256:<64 hex>`. Compose files are bound to the manifest by SHA-256.

Shared test vectors live in `node-agent/internal/release/testdata/signature/`.
They were produced by OpenSSL 3, and both verifiers test against them.

## One-time setup

1. **Signing key.** On the release machine, outside the repository:

   ```sh
   install -d -m 700 ~/.arknet
   /opt/homebrew/opt/openssl@3/bin/openssl genpkey -algorithm ed25519 -out ~/.arknet/release-signing.key
   chmod 600 ~/.arknet/release-signing.key
   scripts/publish-release.py public-key --signing-key ~/.arknet/release-signing.key > deploy/node/release-signing.pub
   ```

   Commit `deploy/node/release-signing.pub`. Keep an offline backup of the
   private key (Prompt 54A covers off-site copies). Anyone holding it can
   publish releases that every Node accepts.
2. **Registry login.** Pushing needs a token with `write:packages`:

   ```sh
   gh auth refresh -h github.com -s write:packages
   gh auth token | docker login ghcr.io -u <github-user> --password-stdin
   ```

3. **Public packages.** GitHub creates new container packages as private. After
   the first push, open each package's settings and set its visibility to
   public, connected to the `ArkNet-Community-Node` repository. Images carry
   `org.opencontainers.image.source`, which links them to the repository.
   `publish-release.py` refuses to write a release unless every image can be
   pulled anonymously.

## Cutting a release

1. **Pick the commit** to release (normally `main`). Do not edit `VERSION`: the
   tag sets the release version, and the build stamps it into every binary,
   image label and the signed manifest. The tree keeps its `-dev` marker,
   which the development manifest and tests rely on.
2. **Run the gate** on that commit: `scripts/check-all.sh --e2e`.
3. **Tag it**: `git tag -a v0.1.0 -m "ArkNet 0.1.0"`.
4. **Publish**:

   ```sh
   scripts/publish-release.py publish --tag v0.1.0 \
       --signing-key ~/.arknet/release-signing.key \
       --site-url https://<release-site> --out ~/arknet-release-site
   ```

   The script:
   - exports the tag with `git archive`, so no working-tree file is used;
   - refuses a tree whose `VERSION` is a different release version (it may be
     the exact version or any `-dev` version) and checks that
     `release-signing.pub` matches the key;
   - builds all six images for both platforms and pushes them;
   - records the digests and checks that each image can be pulled anonymously;
   - writes and signs the Node release, then verifies the site exactly as
     `arknet-node` will.

   An existing `releases/<version>/` is never overwritten.
5. **Upload** the site directory to the release site.
6. **Push the tag.** Tags are never moved once pushed.

`releases/<version>/images.json` records the commit and every image digest,
including Core, tracker and relay, for Core operators.

## Verifying a published release by hand

```sh
curl -fsSO https://<release-site>/releases/0.1.0/release.json
curl -fsSO https://<release-site>/releases/0.1.0/release.json.sig
key=$(cat deploy/node/release-signing.pub)
printf '%s' "$key" | base64 -d | cat <(printf '\x30\x2a\x30\x05\x06\x03\x2b\x65\x70\x03\x21\x00') - > pub.der
openssl pkey -pubin -inform DER -in pub.der -out pub.pem
printf 'arknet.node-release-signature/v1\n' | cat - release.json > message
base64 -d < release.json.sig > sig.bin
openssl pkeyutl -verify -pubin -inkey pub.pem -rawin -in message -sigfile sig.bin
```

## Tests

- `deploy/node/arknet_node_test.py` covers unsigned, wrongly signed, tampered,
  malformed and unpinned releases, plus the shared vectors.
- `node-agent/internal/release` and `internal/updates` cover the same cases for
  discovery.
- `scripts/release_site_test.py` covers the generator, signing, the index and
  site layout, and the rule that every Compose image is pinned (or, in
  development stacks, built from this checkout).
- `scripts/publish_release_test.py` (`ARKNET_TEST_RELEASE_PUBLISH=1`) runs
  `publish-release.py` end to end. It uses a throwaway tag, key and local
  registry.
- `scripts/check-node-upgrade-stack.py` and
  `scripts/check-node-install-vm.py` install and upgrade from signed,
  digest-pinned releases.
