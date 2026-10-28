# Version control conventions

The repository has been under git since the Phase 19A final tree. The first
commit is tagged `v0.1.0-mvp`.

## Branches

- `main` is the integration branch. Every commit on `main` passes the
  component gate: gofmt, `go test ./...` and `go vet ./...` for every Go
  module, `node-ui` tests and build, harness tests, worker tests in the
  transfer image, and `scripts/check-security-hygiene.py`. Run it with
  `scripts/check-all.sh`; runtime changes also need `scripts/check-all.sh --e2e`.
- Work happens on short-lived branches named after the prompt:
  `phase20/<prompt>-<slug>`, for example `phase20/45a-operator-accounts`.
  Fixes outside a prompt use `fix/<slug>`.
- Merge into `main` only after the branch's acceptance criteria have actually
  run. Keep history linear: rebase or squash, with no merge commits on `main`.
- Never force-push `main` or rewrite a commit that a tag or report references.

## Commits

- One logical change per commit. Explain *why* in the body, and name the
  prompt and any ARKSEC ID it addresses.
- Run `python3 scripts/check-security-hygiene.py` before committing. Never
  commit secrets, keys, `.env` files (except `*.env.example`), databases,
  logs, media, `deploy/live-test/secrets/`, `node_modules`, `dist`, `.arknet-*`
  work directories, or local binaries. `.gitignore` enforces these
  categories. If a needed file is ignored, change the rule deliberately in its
  own commit; never use `git add -f`.
- If a secret is ever committed, treat it as exposed: rotate it first, then
  remove it from history.

## Tags

All tags are annotated and are never moved or deleted once pushed.

| Kind | Format | Meaning |
| --- | --- | --- |
| Milestone | `v<major>.<minor>.<patch>-<label>`, e.g. `v0.1.0-mvp` | A validated development milestone. The message names the validation report and its evidence digest. A milestone tag is not a release. |
| Release | `v<version>`, matching `VERSION` and `deploy/node/release.json` | A published whole-stack release. Created only by the release process, after the release's own validation. |

`VERSION` currently reads `0.0.1-dev`. `v0.1.0-mvp` marks the MVP milestone; it
does not change the shipped stack version. Bump `VERSION` and the release
manifest in their own commit when cutting a real release.

## Validation evidence and commits

Validation reports must name the commit they ran on. Before running acceptance
checks, commit the tree and record `git rev-parse HEAD`. A report is valid only
for that commit: any later runtime change requires rerunning the checks.

The Phase 19A report predates git and identifies its tree by a file-hash
manifest digest (`7b71aee9…`). That manifest also covered two untracked local
macOS binaries. Without them, it matches the tagged commit's 371 files exactly.
Over the committed files, the same `tree-hash.sh` gives
`d7be06794a67e268c13351d8575ba8bee4229fbd3ee0f64205263292afa95d61`.
To check it from a clean export:

```sh
git archive v0.1.0-mvp | tar -x -C /path/to/empty-dir
/path/to/tree-hash.sh /path/to/empty-dir /path/to/manifest.txt
```

## Remotes

No remote is configured. Before pushing anywhere, confirm the target is
private. `deploy/live-test` contains private VPN and LAN addresses, which must
be removed before any public publication.
