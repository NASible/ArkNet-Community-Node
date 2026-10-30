# NodeNet operator guide

## Audience and boundaries

This is a private, trusted-operator live-test deployment, not a production
multi-operator trust boundary. One Mac administers the simulated owners User A,
User B, and User C. Core and each Node have separate databases and credentials.
Application-level operator RBAC and the previously documented ARKSEC findings
are not closed by this deployment's firewall/reverse-proxy controls.

Management is available only from the operator Mac's VPN address
`100.64.0.10`. Jellyfin is user-facing on the existing VPN and the local
`192.168.0.0/24` LAN, not the public Internet. Public/shared DNS and the existing
Headscale control plane were not reconfigured. These four new Tailscale clients
use `--netfilter-mode=off` so UFW's explicit SSH/HTTPS/DNS permits actually
apply; automatic Tailscale ACCEPT rules otherwise take precedence over UFW.
SSH is permitted only from the operator Mac. Reapply this deliberate policy
when rebuilding a VM; its non-secret policy is backed up at
`/etc/arknet/network-policy.json`.

## Interfaces

| Owner | Node management | Dedicated Jellyfin |
| --- | --- | --- |
| User A | https://node-a.nodenet.arknet.media | https://jellyfin-a.nodenet.arknet.media |
| User B | https://node-b.nodenet.arknet.media | https://jellyfin-b.nodenet.arknet.media |
| User C | https://node-c.nodenet.arknet.media | https://jellyfin-c.nodenet.arknet.media |

Member APIs (health and member/operator login) are
`https://member-a.nodenet.arknet.media`, `https://member-b.nodenet.arknet.media`
and `https://member-c.nodenet.arknet.media`; management routes answer 404 there.

Core API: `https://core.nodenet.arknet.media`. It does not ship a separate Core
administration dashboard. Private tracker: `https://tracker.nodenet.arknet.media`.

Read credentials locally in `secrets/access.txt` (mode 0600). The complete
machine/account credential state is `secrets/runtime.json` (mode 0600).
Neither file belongs in source archives, chat, or a public issue. Jellyfin
administrator identities are `owner-a`, `owner-b`, and `owner-c`; ArkNet global
identities are `user-a`, `user-b`, and `user-c`, with distinct passwords and
Node-specific pairwise member IDs. Simulated ownership is not an implemented
application OWNER role.

The Mac's project-only resolver is `/etc/resolver/nodenet.arknet.media` and uses
Core's VPN-only DNS service at `100.64.0.21`. The project CA is trusted for SSL on
this Mac. Other client devices need their own approved DNS/trust setup. Core is
currently the only project DNS server: prolonged Core host downtime can affect
uncached hostname lookups even when a Node/Jellyfin remains healthy.

## Media workflow

Each Node has a separate 20 GiB media disk mounted at `/srv/arknet/media` and an
Agent allocation of 20 GiB. Filesystem overhead means usable disk capacity is
slightly smaller than the advertised raw allocation; no promise is made that a
single 20 GiB payload fits. Jellyfin mounts finalized media read-only.

The Media UI's Make Available workflow performs two supported API operations:
selection followed by explicit transfer Start. Selection alone deliberately only
queues an item; it is not an automatic download trigger. `prove-media.py`
exercises the same select-then-start flow.

The generated eight-second test clip is titled **NodeNet generated playback
test** in the ArkNet catalog. It contains generated test video and a tone, not
user media. It has been downloaded through the real private v2 libtorrent
workers from Core's authorized web seed, finalized by each Agent, indexed by
Jellyfin, and direct-streamed with matching SHA-256 and size checks. See
`sample-proof.json` for measured results; no payload was pre-seeded into a
Node's final media directory.

Peer Serving remains OFF, DHT/PEX/LSD remain disabled, and no peer port is
published. Peer-to-peer swarms, interruption/resume, extended Core outages,
unsupported playback codecs, and the complete historic MVP acceptance suite are
not certified by this deployment proof. Jellyfin transcoding is disabled for
the initial small-VM direct-play pilot.

## Backups

Each VM runs `arknet-backup.timer` daily at 04:15/04:25/04:35/04:45
America/Anchorage for Core/A/B/C respectively. It retains seven encrypted local
DB/config archives in `/var/backups/arknet`. PostgreSQL uses a consistent
`pg_dump`; Jellyfin is briefly stopped while copying its local SQLite/config
files, then restarted. Media payloads, Jellyfin cache, and build caches are not
backed up. Node-local data is not copied into Core.

An hourly user LaunchAgent, `media.arknet.nodenet.backup-pull`, copies encrypted
archives to this Mac and checks SHA-256 manifests. It runs only while this Mac's
user session is available and VPN-reachable. The Mac retains fourteen archives
per VM under `~/.local/share/arknet-live-test/backups/<hostname>/`.

Private age identities remain under
`~/.local/share/arknet-live-test/backup-keys/` on this Mac; they are never sent
back to a VM for restore verification. The private CA signing key is also
Mac-only at `~/.local/share/arknet-live-test/pki/`. Protect these keys separately:
there is no configured off-site/disaster-recovery backup of the operator Mac.
An encrypted archive without its age identity cannot be restored.

`verify-backups.py` decrypts the off-VM copy locally, restores PostgreSQL into a
new disposable database on its original VM, checks the restored schema, then
removes the probe database. It checks the Jellyfin SQLite files on disposable
local copies. It never replaces a live database or imports one Node's data into
another system. Platform-specific SQLite SHM files are regenerated, not reused
across Linux/macOS. See `backup-proof.json` for results.

## Maintenance and evidence

The running Compose definition is `/opt/arknet/compose.json`. A byte-identical
`compose.yaml` is shipped for the strict release manifest; do not edit one
without regenerating both. Runtime environment files are root-only. The static
production UI uses a host-mounted Nginx config and Docker's embedded DNS so
Agent container IP changes do not leave a stale proxy destination.

Image IDs and the non-secret source SHA-256 are recorded in `build-record.json`.
Build publication is atomic: `build-progress.json` is not deployable until the
complete image set is built. Older images are retained; no global Docker prune
or unrelated workloads were touched.

Useful verification entry points, run from the repository root:

```sh
python3 deploy/live-test/verify-deployment.py
python3 deploy/live-test/prove-media.py
python3 deploy/live-test/verify-backups.py
```

`verify-reboots.py` actually reboots the four new VMs one at a time; run it only
when interruptions to this test deployment are acceptable. Deployment/source
builders and initial bootstrap scripts are not read-only checks.

Evidence files are scoped to this pilot. They do not replace or rewrite the
historic [MVP validation report](../../docs/mvp-validation-report.md).
