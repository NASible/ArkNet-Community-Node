#!/usr/bin/env bash
# Install a pinned Lima release plus QEMU on an Ubuntu x86_64 CI runner, for
# scripts/check-node-install-vm.py. The tarball is checked against a pinned
# SHA-256 before anything is extracted.
set -euo pipefail

LIMA_VERSION=2.3.0
LIMA_SHA256=cc8ba97f1c801b899f7c9eda9919dfc4ce4fa2c140c53e5add15aa2d0a1936f9
LIMA_ASSET_ID=717479697 # same file through the REST API, if the download CDN fails
archive="lima-${LIMA_VERSION}-Linux-x86_64.tar.gz"

[ "$(uname -s)-$(uname -m)" = "Linux-x86_64" ] || { echo "install-lima.sh supports Linux x86_64 only" >&2; exit 1; }

sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq qemu-system-x86 qemu-utils ovmf

# GitHub-hosted Linux runners expose /dev/kvm; let the runner user open it.
if [ -e /dev/kvm ]; then
    echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"' |
        sudo tee /etc/udev/rules.d/99-kvm4all.rules >/dev/null
    sudo udevadm control --reload-rules
    sudo udevadm trigger --name-match=kvm
else
    echo "warning: /dev/kvm is missing; the VM check will be very slow" >&2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
if ! curl -fsSL --retry 3 --retry-delay 10 --retry-all-errors -o "$work/$archive" \
        "https://github.com/lima-vm/lima/releases/download/v${LIMA_VERSION}/${archive}"; then
    echo "release download failed; trying the REST API asset endpoint" >&2
    curl -fsSL --retry 3 --retry-delay 10 --retry-all-errors -H "Accept: application/octet-stream" -o "$work/$archive" \
        "https://api.github.com/repos/lima-vm/lima/releases/assets/${LIMA_ASSET_ID}"
fi
echo "${LIMA_SHA256}  $work/$archive" | sha256sum -c --quiet
sudo tar -C /usr/local -xzf "$work/$archive"
limactl --version
