# Prompt 51B validation: continuous integration

Decision D13 (confirmed 2026-11-08): GitHub-hosted runners, not a self-hosted
runner, because the repository is public. See [CI](ci.md).

## Delivered

- `.github/workflows/gate.yml`: component gate on every push and pull request;
  `--e2e --vm` nightly, on `v*` tags and on demand. No secrets, read-only
  token, actions pinned by SHA, secret-scanned log artifacts.
- `scripts/check-log-secrets.py` with tests; `scripts/ci_workflow_test.py`
  guarding the workflow rules.
- `scripts/ci/install-lima.sh`: pinned, checksum-verified Lima 2.3.0. It falls
  back to the REST API asset endpoint, because github.com release downloads
  returned HTTP 504 for every release during this work.
- Harness portability for Linux Docker (`scripts/harness_scratch.py`):
  - portable scratch directory;
  - removal of root-owned bind-mount files;
  - host-side servers reachable without `host.docker.internal`;
  - `arknet-node` run as root by the upgrade check;
  - test-built transfer images.
- A fix to the item 11 leak check, which failed about one run in four: a 4-byte
  address matched by chance inside ~150 MB of compressed video. A binary match
  is now excused only when it, plus 28 bytes on each side, occurs verbatim in a
  media file the run generated. Text forms are always refused. Five mutations
  are caught.

## Rehearsal on a GitHub-like Linux runner

A throwaway Proxmox VM (`arknet-ci-probe`, VMID 113: Ubuntu 24.04, 4 vCPU,
8 GiB, CPU type `host` for KVM) ran the workflow's commands. It had Docker CE
29.9, Go 1.26.10, Node 26.13, Python 3.12 and OpenSSL 3.0.

| Run | Commit | Result |
| --- | --- | --- |
| component gate (first) | `5efc6fe` | 13/14, harness-tests failed: hard-coded `~/.hermes` scratch path |
| component gate | `b4fb36f` | **14/14 PASS**, logs clean, no stray directories |
| `--e2e` (first) | `b4fb36f` | 15/18: ffmpeg missing, Mac-only image tag, root-owned leftovers, `host.docker.internal` |
| `--e2e` | `b4e621f` | **18/18 PASS**, 20 log files clean, 0 leftover directories |
| `--vm` install check | `759ea15` | **21 PASS, ALL PASS**, no Lima VMs left |

The macOS full gate also passed on `9f9032d` (18/18, logs clean), so the Linux
changes did not break the Mac path.

## Acceptance (needs the operator to push)

- [ ] One real GitHub runner execution of the component gate ends `EXIT 0`.
  Link: _to be added_.
- [ ] A deliberately failing branch shows a failed run with the failing step.
  Link: _to be added_.
