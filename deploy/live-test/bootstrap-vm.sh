#!/usr/bin/env bash
# Bootstrap only newly created nodenet VMs. No credentials belong here.
set -euo pipefail
case "$(hostname)" in nodenet-core|nodenet-node-a|nodenet-node-b|nodenet-node-c) ;; *) echo 'Unexpected hostname' >&2; exit 1 ;; esac
: "${BOOTSTRAP_SSH_IP:?Set the current operator LAN IP for temporary SSH access}"
# Proxmox emits a deprecated cloud-init user field: exit 2 means completed
# with recoverable warnings. Still reject real errors or unfinished bootstrap.
python3 -c 'import json,subprocess; p=subprocess.run(["cloud-init","status","--wait","--format","json"],capture_output=True,text=True); s=json.loads(p.stdout); assert p.returncode in (0,2) and s.get("status")=="done" and not s.get("errors"), "Cloud-init did not finish successfully"'
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq docker.io docker-compose-v2 qemu-guest-agent nginx age ufw curl ca-certificates gnupg
systemctl disable --now nginx
systemctl enable --now docker qemu-guest-agent
# Ubuntu 24.04 package source published by Tailscale; no auth keys embedded.
install -d -m 0755 /usr/share/keyrings
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg -o /usr/share/keyrings/tailscale-archive-keyring.gpg
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list -o /etc/apt/sources.list.d/tailscale.list
apt-get update -qq
apt-get install -y -qq tailscale
systemctl enable --now tailscaled
install -d -m 0755 /opt/arknet /srv/arknet
install -d -m 0700 /var/backups/arknet
if [[ "$(hostname)" != nodenet-core ]]; then
  device=/dev/sdb
  [[ "$(blockdev --getsize64 "$device")" == 21474836480 ]] || { echo 'Unexpected media disk size' >&2; exit 1; }
  if ! findmnt -rn /srv/arknet/media >/dev/null; then
    if [[ -n "$(wipefs --no-act --noheadings --output TYPE "$device")" ]]; then
      echo 'Media disk has an existing signature; refusing to format' >&2; exit 1
    fi
    mkfs.ext4 -q -m 0 -L arknet-media "$device"
    install -d -m 0755 /srv/arknet/media
    uuid=$(blkid -s UUID -o value "$device")
    printf 'UUID=%s /srv/arknet/media ext4 defaults,noatime 0 2\n' "$uuid" >> /etc/fstab
    mount /srv/arknet/media
  fi
fi
# Docker ports still require explicit loopback binding: UFW alone does not
# filter Docker's published ports. Management must not publish to 0.0.0.0.
ufw default deny incoming
ufw default allow outgoing
ufw allow from "$BOOTSTRAP_SSH_IP" to any port 22 proto tcp comment 'Temporary operator bootstrap'
ufw allow in on tailscale0 comment 'Authorized tailnet traffic; tailnet ACLs also required'
ufw allow 41641/udp comment 'Encrypted Tailscale transport'
ufw --force enable
printf '\nBOOTSTRAP_OK %s\n' "$(hostname)"
docker --version
docker compose version
systemctl is-active docker qemu-guest-agent tailscaled
if [[ "$(hostname)" != nodenet-core ]]; then findmnt -no SOURCE,TARGET,FSTYPE /srv/arknet/media; fi
