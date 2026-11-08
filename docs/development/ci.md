# Continuous integration

The gate (`scripts/check-all.sh`) runs on GitHub-hosted runners through
[`.github/workflows/gate.yml`](../../.github/workflows/gate.yml) (decision D13).

| Job | When | Command |
| --- | --- | --- |
| component gate | every branch push, every pull request, manual runs | `scripts/check-all.sh` |
| full gate | nightly at 02:00 AKST, every `v*` tag, manual runs with *full* ticked | `scripts/check-all.sh --e2e --vm` |

Each job uploads its step logs and diagnostics as an artifact (14 days).

## Why GitHub-hosted, not self-hosted

The repository is public. On a self-hosted runner, a pull request could run its
code on your hardware and network. GitHub-hosted runners give every job a
fresh, discarded VM, are free for public repositories, and provide Docker and
KVM for nested VMs.

## No secrets on the runner

- The workflow uses no repository secrets, and a test (`scripts/ci_workflow_test.py`)
  refuses any `secrets.` reference and any `pull_request_target` trigger.
- The job token is read-only (`permissions: contents: read`), and checkout does
  not keep it (`persist-credentials: false`).
- Every harness creates throwaway database passwords, signing keys, admin
  tokens and setup codes, and destroys them with its stacks.
- Artifacts of a public repository are public. Before upload,
  `scripts/check-log-secrets.py` scans the logs for capability strings, admin
  tokens, private keys, setup codes and database passwords. It names only the
  file and kind, never the value. If it finds anything, the upload step is
  skipped and the job fails.
- Third-party actions are pinned by commit SHA; the Lima download is pinned by
  version and SHA-256.

## Host capabilities

The jobs install everything they need on `ubuntu-24.04`:

| Need | Component gate | Full gate | Source |
| --- | --- | --- | --- |
| Docker Engine with Compose and Buildx | yes | yes | preinstalled on the runner |
| Go 1.26, Node 26 | yes | yes | `actions/setup-go`, `actions/setup-node` |
| Python 3 (stdlib only), OpenSSL 3 | yes | yes | preinstalled |
| ffmpeg | | yes | apt |
| Lima, QEMU, KVM | | yes (`--vm`) | `scripts/ci/install-lima.sh` |
| `sudo` without a password | | yes | preinstalled; the upgrade check runs `arknet-node` as root, as operators do |

Playwright with Chromium is needed only by `scripts/check-operator-https.py`,
which is not a gate step and does not run in CI.

### Linux notes

The harnesses were first written on macOS, where Docker Desktop and Colima hide
two Linux behaviours. They now handle both:

- `host.docker.internal` does not exist on Linux Docker. Host-side test servers
  listen on the Docker bridge gateway instead
  (`scripts/harness_scratch.py:host_for_containers`).
- Containers write root-owned files into bind mounts.
  `harness_scratch.remove_tree` removes them with a network-less container
  using the digest-pinned Postgres image the stacks already pull.

Scratch files go to `$ARKNET_SCRATCH` if set, otherwise the system temporary
directory (`harness_scratch.scratch_root`).

## Running a full gate by hand

In GitHub: **Actions → gate → Run workflow**, tick *Also run --e2e --vm*.

## Rehearsing on your own Linux VM

On a fresh Ubuntu 24.04 VM with nested virtualisation (Proxmox: CPU type
`host`): install Docker CE, Go 1.26, Node 26, ffmpeg and `libatomic1`, add the
user to the `docker` group, then run `scripts/ci/install-lima.sh` and
`scripts/check-all.sh --e2e --vm`.
