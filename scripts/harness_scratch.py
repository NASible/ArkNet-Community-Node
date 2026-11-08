"""Where test harnesses put temporary files.

Order: `ARKNET_SCRATCH` (created if needed), then the maintainer's Hermes
scratch directory if it already exists (macOS' per-user TMPDIR is not always
shared with Docker Desktop or Colima), then the system temporary directory.
CI runners and other machines use the system temporary directory.

`remove_tree` deletes a harness directory even when containers wrote
root-owned files into its bind mounts, which happens on Linux Docker (Docker
Desktop and Colima remap ownership, so it is invisible on a Mac).

`as_operator` runs arknet-node as root on Linux, as operators do: the UI
container drops every capability and can only read a root-owned TLS key.

`host_for_containers` says where a host-side test server must listen and
which address containers dial to reach it.
"""
from __future__ import annotations

import ipaddress
import os
import platform
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def scratch_root() -> Path:
    explicit = os.environ.get("ARKNET_SCRATCH")
    if explicit:
        path = Path(explicit)
        path.mkdir(parents=True, exist_ok=True)
        return path
    hermes = Path.home() / ".hermes" / "cache" / "scratch"
    if hermes.is_dir():
        return hermes
    return Path(tempfile.gettempdir())


def cleanup_image() -> str:
    """The digest-pinned Postgres image every test stack already pulls."""
    match = re.search(r"image:\s*(postgres:\S+@sha256:[0-9a-f]{64})", (ROOT / "deploy/core/compose.yaml").read_text())
    if not match:
        raise RuntimeError("deploy/core/compose.yaml has no digest-pinned postgres image")
    return match.group(1)


def remove_tree(path: Path) -> None:
    path = Path(path)
    if not path.exists():
        return
    try:
        shutil.rmtree(path)
        return
    except PermissionError:
        pass
    target = path.resolve()
    subprocess.run(["docker", "run", "--rm", "--network=none", "--user=0", "--entrypoint=sh",
                    "-v", f"{target}:/w", cleanup_image(), "-c", "rm -rf /w/* /w/.[!.]* /w/..?*"],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False, timeout=120)
    shutil.rmtree(path)


def host_for_containers() -> tuple[str, str]:
    """(bind address, address containers dial) for a host-side test server.

    Docker Desktop and Colima forward host.docker.internal to the Mac's
    loopback. Linux Docker has no such name: listen on the default bridge
    gateway, which every container network can route to.
    """
    if platform.system() != "Linux":
        return "127.0.0.1", "host.docker.internal"
    result = subprocess.run(["docker", "network", "inspect", "bridge", "--format",
                             "{{range .IPAM.Config}}{{.Gateway}} {{end}}"],
                            capture_output=True, text=True, check=False, timeout=30)
    for candidate in result.stdout.split() if result.returncode == 0 else []:
        try:
            address = ipaddress.ip_address(candidate)
        except ValueError:
            continue
        if address.version == 4 and address.is_private and not (address.is_unspecified or address.is_loopback):
            return str(address), str(address)
    raise RuntimeError("cannot find the Docker bridge gateway for host-side test servers")


def as_operator(env: dict) -> list[str]:
    """Command prefix that runs arknet-node with root privileges on Linux.

    Docker Desktop and Colima hide host file ownership, so macOS needs none.
    On Linux a non-root caller uses passwordless sudo (CI runners have it) and
    keeps only PATH, which carries the OpenSSL 3 the tool needs.
    """
    if platform.system() != "Linux" or os.geteuid() == 0:
        return []
    return ["sudo", "-n", "env", "PATH=" + env.get("PATH", os.defpath)]
