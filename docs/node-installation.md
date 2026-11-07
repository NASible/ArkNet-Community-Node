# Installing an ArkNet Node

This is the supported way to turn a fresh Ubuntu LTS machine into an ArkNet
Node. You need only a shell on that machine. No repository clone, Compose
editing, or Docker knowledge is required.

## Requirements

| | Minimum |
|---|---|
| Operating system | Ubuntu Server LTS 22.04, 24.04 or 26.04 |
| CPU architecture | amd64 (x86-64) or arm64 (AArch64) |
| Memory | 2 GB |
| Free disk | 20 GiB in the install directory **and** under `/var/lib`, where Docker keeps images |
| Network | Outbound HTTPS to the ArkNet release site, the image registry and ArkNet Core. No inbound ports are needed. |
| Access | A user with `sudo` |

The installer checks all of these and stops with an explanation if one is
missing.

## Install

```bash
curl -fsSLO https://<arknet-release-site>/install.sh
less install.sh          # optional: read what it will do
sudo sh install.sh --core-url https://<arknet-core>
```

The installer goes through these steps:

1. **Checks the machine**: Ubuntu LTS version, architecture, memory and free
   disk.
2. **Installs or validates Docker.**
   - If Docker is missing it installs `docker.io` and `docker-compose-v2` from
     the Ubuntu archive.
   - It requires the Compose plugin ≥ 2.20 and enables the daemon.
   - `--no-install-docker` only validates an existing installation.
3. **Creates `/opt/arknet`.**
   - The directory and `releases/` are 0755, and `backups/` is 0700.
   - It writes `.env` (0600) with a random PostgreSQL password, your Core URL,
     the release index URL and the release signing public key
     (`ARKNET_RELEASE_VERIFY_KEY`) built into this `install.sh`.
   - Existing settings are never overwritten.
4. **Installs `arknet-node`** to `/usr/local/bin`.
5. **Downloads the newest stable release.**
   - `arknet-node install` fetches the channel index and checks each SHA-256:
     index → manifest → Compose files.
   - It verifies the manifest's Ed25519 signature (`release.json.sig`) with
     the signing key, and requires every bundled image to be pinned by digest.
     It prints `signature verified (release key SHA256:…)`.
   - It validates the manifest and pulls the pinned images.
6. **Starts the Node stack** and validates it. The Agent must be healthy and
   running the downloaded release, and Jellyfin must be healthy.
7. **Reports the Node state** and how to reach the setup UI.

On success the output ends like this:

```text
INSTALLED: ArkNet 0.1.0 (stable) is running.
Node ID: node_…
Node state: ENROLLMENT_PENDING (enrollment requested; an ArkNet Core administrator must approve this Node)
Setup UI: http://127.0.0.1:5174 (local to this machine only).
  From another computer: ssh -L 5174:127.0.0.1:5174 <user>@<this-machine>, then open http://localhost:5174
```

## After installing

- **Enrollment:** the Node has asked Core to enroll it. A Core administrator
  approves it by Node ID, after which the state becomes `CONNECTED`. Without
  `--core-url` the Node runs locally and stays in `PROVISIONING`.
- **Management UI:** open `https://<node-address>:8443` on your LAN. The
  installer generates a per-Node self-signed certificate and prints its SHA-256
  fingerprint alongside the setup code. Compare the fingerprint with the
  browser's certificate details before accepting the certificate warning.
  Plain HTTP remains loopback-only: for an SSH tunnel, use
  `ssh -L 5174:127.0.0.1:5174 user@node`, then browse to
  `http://localhost:5174`. Operator sign-in is required for management actions.
- **Releases:** run `sudo arknet-node status`, `upgrade`, or `rollback`. See
  [Node upgrade](node-upgrade.md).
- **Operator accounts:** `sudo arknet-node operator setup-code` prints a
  one-time, 1-hour code for creating the first operator (password plus an
  authenticator app). `operator recover NAME`, `operator list` and
  `operator disable NAME` handle lost access, all from the local shell. Codes
  are printed to the terminal only and never logged. In the UI choose **Set up
  operator**, enter the code, choose a username and password (at least 12
  characters), and add the displayed key to an authenticator app. Confirm the
  current six-digit code, then save the one-time recovery codes somewhere safe.
  Sign in with your password plus an authenticator code (or a recovery code).
  **Sign out** revokes the server-side session. The UI stores no operator data
  in localStorage or sessionStorage. See
  [Node operator authentication](node-operator-authentication.md).
- **HTTPS certificate:** `/opt/arknet/tls/key.pem` is mode 0600 and readable by
  the UI container user; `cert.pem` is public. Both stay on the Node, mounted
  read-only into the UI. `sudo arknet-node tls show` prints the fingerprint.
  `sudo arknet-node tls renew` explicitly replaces the certificate after an
  address change or before its 825-day expiry; it restarts the installed UI
  with the new certificate. Compare the new
  fingerprint before accepting it. Certificates are not silently replaced.

## Options

| Option | Meaning |
|---|---|
| `--core-url URL` | ArkNet Core to enroll with. Must be an HTTPS base URL, not this machine's loopback. |
| `--release-url URL` | Release site. Defaults to the site this `install.sh` was downloaded from, which is set when the release site is published. |
| `--channel stable\|development` | Release channel. Defaults to `stable`. |
| `--home DIR` | Installation directory. Defaults to `/opt/arknet`. |
| `--no-install-docker` | Validate Docker but never install it. |
| `--allow-insecure-http` | **Development and testing only.** Accepts plain-HTTP Core and release URLs. |

## Re-running

Re-running the installer is safe and is the fix for most interrupted installs.
It:

- keeps `.env`, all data volumes and the installed release;
- refreshes `arknet-node`;
- restarts and re-validates the installed release. It does not upgrade; use
  `arknet-node upgrade` for that.

A failed first install records nothing, so the next run starts the install
again.

## Errors

Every failure prints `INSTALL FAILED: <reason>` and the next step, for example:

| Message | What to do |
|---|---|
| `must run as root` | Prefix with `sudo`. The message repeats your options. |
| `Unsupported operating system: …` | Use Ubuntu Server LTS 22.04, 24.04 or 26.04. |
| `Unsupported CPU architecture` | Use an amd64 or arm64 machine. |
| `Not enough memory` / `Not enough free disk space on …` | Add memory or disk. The message says which path. |
| `Docker Compose X is too old` | Install a current Docker Engine and Compose plugin, then re-run. |
| `--core-url must use https://` | Use Core's HTTPS URL. |
| `--core-url points at this machine's loopback address` | The Agent runs in a container; use Core's network address. |
| `FAILED at download: … digest does not match` | The release site content is inconsistent. Do not continue; report it. |
| `FAILED at download: Release signature rejected` / `unavailable` | The release is not signed by the ArkNet release key. Do not continue; report it. |
| `OpenSSL 3 or newer is required` | Supported Ubuntu releases ship OpenSSL 3; install the `openssl` package. |
| `does not contain the ArkNet release signing key` | Download `install.sh` from the release site, not from the repository. |
| `FAILED at images: Could not pull …` | Check registry access, then re-run. Nothing was started. |
| After installing: something stops working | Open **Diagnostics** in the setup UI; it names the failing service and what to do. See [Node diagnostics](node-diagnostics.md). |
| `INSTALL FAILED while starting the Node` | The output includes the `docker compose … logs` command for the failing service. Fix the cause and re-run. |
| `WARNING: ArkNet Core … has not accepted the enrollment request yet` | Check `ARKNET_CORE_URL` in `/opt/arknet/.env` and network access. The Agent keeps retrying. |

## Publishing a release site

The site serves:

- `install.sh`, with `__ARKNET_RELEASE_URL__` replaced by the site's own URL
  and `__ARKNET_RELEASE_VERIFY_KEY__` by the release signing public key;
- `arknet-node`, `release-signing.pub` and `index.json`;
- `releases/<version>/` with the signed, digest-pinned `release.json`,
  `release.json.sig`, its Compose files and `images.json`.

`scripts/publish-release.py` writes it; see the
[release process](release-process.md). `scripts/check-node-install-vm.py`
installs from such a site on a fresh VM.

## Security notes

- The installer is downloaded first rather than piped into a shell, so you can
  read it before running it as root.
- `arknet-node` comes from the same HTTPS site as the installer.
- Every release manifest must be signed by the ArkNet release key, whose
  public half is built into `install.sh` and stored in `.env`. Compose files
  are bound to the signed manifest by SHA-256, and every bundled image is
  pinned by digest. The index is not signed: a hostile release site can
  withhold updates or point at an older signed release, but `arknet-node`
  never installs a version older than the running one.
- Nothing listens publicly. The Agent, UI and Jellyfin are bound to loopback
  by the release Compose file, and enrollment is outbound to Core.
- The Agent has two loopback ports: management on `127.0.0.1:8082` (operator
  and management APIs) and member on `127.0.0.1:8086` (`ARKNET_MEMBER_PORT`:
  health and member/operator login). Routes answer `404` on the wrong port, so
  a reverse proxy can expose the member port without exposing management. The
  member port still serves operator sign-in and first-operator setup, so block
  `/api/v1/operator/` at that proxy unless operators need it there.
