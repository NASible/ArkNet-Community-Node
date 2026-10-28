#!/usr/bin/env python3
"""Validate the Node development Compose contract without starting containers."""
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parent.parent
DEPLOY = ROOT / "deploy" / "node"


def main():
    env = dict(os.environ, POSTGRES_PASSWORD="compose-contract-test-only")
    result = subprocess.run(
        ["docker", "compose", "config", "--format", "json"],
        cwd=DEPLOY, env=env, capture_output=True, text=True,
    )
    if result.returncode:
        raise AssertionError("Compose config failed: " + result.stderr.strip())
    config = json.loads(result.stdout)
    services = config["services"]
    assert set(services) == {"arknet-agent", "arknet-ui", "jellyfin", "postgres"}
    assert not services["postgres"].get("ports"), "PostgreSQL must not publish a host port"
    assert set(config["networks"]) == {"media-services", "node"}
    assert config["networks"]["node"]["driver"] == "bridge"
    assert config["networks"]["media-services"]["driver"] == "bridge"
    assert set(services["postgres"]["networks"]) == {"node"}
    assert set(services["arknet-ui"]["networks"]) == {"node"}
    assert set(services["jellyfin"]["networks"]) == {"media-services"}
    assert set(services["arknet-agent"]["networks"]) == {"media-services", "node"}
    for name, service in services.items():
        assert service.get("healthcheck"), name + " needs a health check"
    volumes = services["postgres"]["volumes"]
    assert len(volumes) == 1
    assert volumes[0]["type"] == "volume" and volumes[0]["source"] == "postgres-data"
    assert volumes[0]["target"] == "/var/lib/postgresql", "Postgres 18 volume layout"
    assert "postgres-data" in config["volumes"]
    agent = services["arknet-agent"]
    ui = services["arknet-ui"]
    jellyfin = services["jellyfin"]
    assert agent["environment"]["ARKNET_AGENT_ADDR"] == ":8081"
    assert agent["environment"]["PGHOST"] == "postgres"
    assert agent["environment"]["PGPORT"] == "5432"
    assert agent["environment"]["PGPASSWORD"] == services["postgres"]["environment"]["POSTGRES_PASSWORD"]
    assert ui["environment"]["ARKNET_AGENT_URL"] == "http://arknet-agent:8081"
    assert agent["environment"]["JELLYFIN_API_URL"] == "http://jellyfin:8096"
    assert not any(key.startswith("PG") or key == "DATABASE_URL" for key in jellyfin["environment"])
    assert agent["depends_on"]["postgres"]["condition"] == "service_healthy"
    assert ui["depends_on"]["arknet-agent"]["condition"] == "service_healthy"
    for service, target in [(agent, 8081), (ui, 5173), (jellyfin, 8096)]:
        ports = service["ports"]
        assert len(ports) == 1 and ports[0]["host_ip"] == "127.0.0.1"
        assert ports[0]["target"] == target
    jellyfin_mounts = {mount["target"]: mount for mount in jellyfin["volumes"]}
    assert jellyfin_mounts["/config"]["source"] == "appdata"
    assert jellyfin_mounts["/cache"]["source"] == "jellyfin-cache"
    assert jellyfin_mounts["/srv/arknet/media"]["source"] == "media"
    assert jellyfin_mounts["/srv/arknet/media"]["read_only"] is True
    assert jellyfin_mounts["/srv/arknet/transfers"]["source"] == "transfers"
    assert jellyfin["restart"] == "unless-stopped"
    for component in ["node-agent", "node-ui"]:
        assert (ROOT / component / "Dockerfile").is_file()
        assert (ROOT / component / ".dockerignore").is_file()
    example = (DEPLOY / ".env.example").read_text()
    assert "POSTGRES_PASSWORD=\n" in example, "Example must not ship a password"
    for password in [None, ""]:
        without_password = dict(os.environ)
        without_password.pop("POSTGRES_PASSWORD", None)
        if password is not None:
            without_password["POSTGRES_PASSWORD"] = password
        rejected = subprocess.run(
            ["docker", "compose", "--env-file", os.devnull, "config", "--quiet"],
            cwd=DEPLOY, env=without_password, capture_output=True, text=True,
        )
        assert rejected.returncode != 0 and "POSTGRES_PASSWORD" in rejected.stderr, "Require an explicit local password"
    print("PASS: four services, isolated PostgreSQL/Jellyfin networks, named storage, proxy and DB configuration")
    print("PASS: Jellyfin config persists, media is read-only, and Agent health polling is configured")
    print("PASS: startup configuration rejects an unset or empty database password")


if __name__ == "__main__":
    main()
