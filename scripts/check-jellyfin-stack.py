#!/usr/bin/env python3
"""Verify the disposable Node Jellyfin stack and remove every test resource."""
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import time
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent
COMPOSE = ROOT / "deploy" / "node" / "compose.yaml"


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return str(sock.getsockname()[1])


def main():
    project = "arknet-jellyfin-test-" + uuid.uuid4().hex[:12]
    ui_port, agent_port, jellyfin_port = free_port(), free_port(), free_port()
    env = dict(
        os.environ,
        POSTGRES_PASSWORD=secrets.token_hex(32),
        POSTGRES_USER="arknet",
        POSTGRES_DB="arknet_node",
        PGSSLMODE="disable",
        ARKNET_UI_PORT=ui_port,
        ARKNET_AGENT_PORT=agent_port,
        ARKNET_CORE_URL="",
        JELLYFIN_BIND="127.0.0.1:" + jellyfin_port,
    )

    def compose(*args, capture=False, check=True):
        result = subprocess.run(
            ["docker", "compose", "-p", project, "-f", str(COMPOSE), *args],
            cwd=ROOT,
            env=env,
            check=check,
            capture_output=capture,
            text=True,
        )
        return result.stdout.strip() if capture else None

    def container(service):
        return compose("ps", "-q", service, capture=True)

    def inspect(service):
        output = subprocess.run(
            ["docker", "inspect", container(service)],
            check=True,
            capture_output=True,
            text=True,
        ).stdout
        return json.loads(output)[0]

    def status():
        with urllib.request.urlopen(
            "http://127.0.0.1:" + ui_port + "/api/v1/status", timeout=3
        ) as response:
            assert response.status == 200
            return json.load(response)

    def wait(label, predicate, timeout=60):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                value = predicate()
                if value:
                    return value
            except (OSError, ValueError):
                pass
            time.sleep(0.5)
        raise AssertionError("Timed out: " + label)

    try:
        compose("up", "--build", "-d", "--wait", "--wait-timeout", "180")

        healthy = wait(
            "UI reports Jellyfin healthy",
            lambda: (lambda value: value if value.get("jellyfin_healthy") is True else None)(status()),
        )
        assert healthy["core_connectivity"] == "NOT_IMPLEMENTED"
        assert healthy["agent_healthy"] is True
        assert healthy["database_healthy"] is True

        jellyfin = inspect("jellyfin")
        postgres = inspect("postgres")
        agent = inspect("arknet-agent")
        jellyfin_networks = set(jellyfin["NetworkSettings"]["Networks"])
        postgres_networks = set(postgres["NetworkSettings"]["Networks"])
        agent_networks = set(agent["NetworkSettings"]["Networks"])
        assert jellyfin_networks.isdisjoint(postgres_networks)
        assert jellyfin_networks <= agent_networks
        assert postgres_networks <= agent_networks

        mounts = {mount["Destination"]: mount for mount in jellyfin["Mounts"]}
        assert mounts["/config"]["RW"] is True
        assert mounts["/cache"]["RW"] is True
        assert mounts["/srv/arknet/media"]["RW"] is False
        assert mounts["/srv/arknet/transfers"]["RW"] is True
        assert jellyfin["HostConfig"]["RestartPolicy"]["Name"] == "unless-stopped"
        serialized = json.dumps(jellyfin["Config"]["Env"]).lower()
        for prohibited in ("postgres", "pgpassword", "database_url"):
            assert prohibited not in serialized

        marker = "arknet-jellyfin-persistence-" + uuid.uuid4().hex
        subprocess.run(
            ["docker", "exec", container("jellyfin"), "/bin/sh", "-c", "printf %s '" + marker + "' > /config/.arknet-persistence-check"],
            check=True,
        )
        compose("rm", "-sf", "jellyfin")
        compose("up", "-d", "--wait", "--wait-timeout", "120", "jellyfin")
        persisted = subprocess.run(
            ["docker", "exec", container("jellyfin"), "cat", "/config/.arknet-persistence-check"],
            check=True,
            capture_output=True,
            text=True,
        ).stdout
        assert persisted == marker
        wait("UI health after Jellyfin recreation", lambda: status().get("jellyfin_healthy") is True)

        print("PASS: Jellyfin starts healthy without Core and is reported by the UI")
        print("PASS: configuration persists across container recreation")
        print("PASS: media is read-only and PostgreSQL is network-isolated from Jellyfin")
    finally:
        compose("down", "--volumes", "--remove-orphans", check=False)
        print("Removed only disposable Jellyfin resources: " + project)


if __name__ == "__main__":
    main()
