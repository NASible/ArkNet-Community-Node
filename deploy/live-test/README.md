# Private NodeNet live-test deployment

Core plus three Community Nodes are deployed on four dedicated Ubuntu 24.04 VMs.
This is separate from NASible and remains a **private trusted-operator pilot**,
not a claim of production or hostile multi-operator readiness.

Start with the [operator guide](OPERATOR-GUIDE.md). Machine details are in
[inventory.json](inventory.json). Credentials are only in the ignored,
owner-readable `secrets/access.txt` and `secrets/runtime.json` files.

## Interfaces and machines

| VMID | Machine / simulated owner | VPN IPv4 | Current LAN address | Interface |
| --- | --- | --- | --- | --- |
| 108 | Core / User A | 100.64.0.21 | 192.168.0.144 | https://core.nodenet.arknet.media/health |
| 109 | Node A / User A | 100.64.0.22 | 192.168.0.86 | https://node-a.nodenet.arknet.media |
| 110 | Node B / User B | 100.64.0.23 | 192.168.0.113 | https://node-b.nodenet.arknet.media |
| 111 | Node C / User C | 100.64.0.24 | 192.168.0.212 | https://node-c.nodenet.arknet.media |

Dedicated Jellyfin interfaces are `https://jellyfin-a.nodenet.arknet.media`,
`https://jellyfin-b.nodenet.arknet.media`, and
`https://jellyfin-c.nodenet.arknet.media`. Core exposes an API, not a separate
Core administration dashboard. The private tracker is
`https://tracker.nodenet.arknet.media`.

All machines use Proxmox `pve2` / `ceph-docker`, 2 vCPU, 2048 MiB RAM, and a
32 GiB system disk. Each Community Node has an additional 20 GiB ext4 media disk,
persistently mounted at `/srv/arknet/media`, with a 20 GiB Agent allocation.
Actual usable filesystem capacity is slightly below the raw allocation.

## Verified application behavior

- Core, private tracker, three Agents, real libtorrent workers, static production
  UIs, separate PostgreSQL databases, and three dedicated Jellyfins are running.
- All Nodes are approved and CONNECTED. Each has its own User A/B/C global
  account, Node-specific membership, verified pairwise login, and a distinct
  Jellyfin administrator. Simulated ownership is not application OWNER RBAC.
- The actual CSS/JavaScript URLs returned by each deployed UI are available and
  applied in a real browser; application status APIs report real service health.
- A generated eight-second clip was downloaded from Core's authorized HTTPS
  web seed to each real private-v2 worker, finalized by its Agent, automatically
  indexed by Jellyfin, and direct-streamed with matching SHA-256 and size.
  Selection alone queues; the Media UI intentionally sends an explicit Start
  action after selection. The proof follows that workflow, not an assumed
  automatic queue-to-download transition.
- Each VM was actually rebooted and recovered its stack. Health, access
  boundaries, retained sample files, and direct streams were checked afterward.
- Encrypted DB/config backups ran on all four VMs, were copied off-VM to this
  Mac, decrypted locally, and restored into disposable PostgreSQL databases.
  Jellyfin SQLite backups passed integrity checks on disposable copies.

See the measured evidence, rather than treating an infrastructure HTTP 200 as
application proof:

- [Deployment and access boundaries](deployment-proof.json)
- [Browser dashboards, release display, and completed media](browser-proof.json)
- [Real media transfer/direct-stream proof](sample-proof.json)
- [Actual VM reboot/recovery proof](reboot-proof.json)
- [Off-VM decryption and restore proof](backup-proof.json)
- [Built image IDs and source SHA-256](build-record.json)

The Jellyfin adapter now uses safely quoted `Authorization: MediaBrowser ...`
authentication, compatible with Jellyfin 12.2. Agent regression tests and vet
passed. PostgreSQL and Jellyfin use immutable registry digest pins; ArkNet
image IDs are recorded and checked against the running containers.

## Access, DNS, and key handling

Management HTTPS and SSH are restricted to the operator Mac's VPN address
`100.64.0.10`. Jellyfin alone is also exposed to the local
`192.168.0.0/24` LAN. Docker-published application ports bind to loopback.
Node A was denied Node B management and Core administration; cross-machine SSH
was verified blocked. No public/shared DNS records or Internet forwarding were
created.

Tailscale's automatic netfilter ACCEPT rules otherwise bypass UFW. These four
new VM clients use `--netfilter-mode=off`, letting their explicit UFW rules
apply. Mode changes were protected by timed rollback and verified using fresh
Mac connections. No shared Headscale ACLs, routes, or existing hosts changed.
Recreated hosts must reapply this policy; see `harden-management.py` and each
VM's `/etc/arknet/network-policy.json`.

The active controller was independently verified on `rknt-dc-ingress-2` against
`https://headscale.arknet.media`; the stale instance on `rknt-net2` was not
reconfigured. Both the unused enrollment key and the subsequently used correct
short-lived key were expired and verified. Temporary raw copies were removed.
The Nodes retain their registered identities.

The operator Mac has an explicitly approved SSL-trusted project CA and a
supplemental resolver only for `nodenet.arknet.media`, using VPN-only DNS at
`100.64.0.21`. Native name resolution and HTTPS were verified without address or
CA overrides. The public CA is [nodenet-root-ca.crt](nodenet-root-ca.crt).
The signing key remains Mac-only outside the repository at
`~/.local/share/arknet-live-test/pki/`. Each VM receives only its own leaf key.

Dedicated operator SSH key: `~/.ssh/arknet-live-test`. Bootstrap host keys are
pinned in `~/.ssh/arknet-live-test-known_hosts`. Example:

```sh
ssh -i ~/.ssh/arknet-live-test -o IdentitiesOnly=yes \
  -o UserKnownHostsFile=~/.ssh/arknet-live-test-known_hosts \
  -o HostKeyAlias=192.168.0.144 arknet@100.64.0.21
```

## Backups and remaining limits

Daily per-VM backup timers retain seven encrypted DB/config archives. An hourly
Mac user LaunchAgent copies/checks encrypted archives and retains fourteen per
VM. Node-local data never goes into Core. The Mac must be awake/logged in and
VPN-reachable for the off-VM pull. Keys, schedules, restoration boundaries, and
excluded data are documented in the [operator guide](OPERATOR-GUIDE.md).

Peer Serving remains OFF; peer ports are not published. P2P swarms, transfer
interruption/resume, extended Core outages, all codecs, and the complete historic
MVP suite are not certified by these deployment checks. Transcoding is disabled
for this direct-play pilot. Core is the single project DNS server; other client
devices need their own approved DNS/trust installation. The operator Mac's CA
and backup identities still need a separately protected disaster-recovery copy.

Previously documented ARKSEC limitations remain open. The historic
[MVP validation report](../../docs/mvp-validation-report.md) is not rewritten or
closed by this live deployment evidence. Do not expose this pilot publicly.
