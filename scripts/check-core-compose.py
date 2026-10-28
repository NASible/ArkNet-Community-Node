#!/usr/bin/env python3
"""Validate the Core development Compose contract without starting containers."""
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parent.parent
DEPLOY = ROOT / "deploy" / "core"


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
    assert set(services) == {"arknet-core", "postgres"}
    assert not services["postgres"].get("ports"), "PostgreSQL must not publish a host port"
    assert set(config["networks"]) == {"core"}
    assert config["networks"]["core"]["driver"] == "bridge"
    for name, service in services.items():
        assert set(service["networks"]) == {"core"}, name
        assert service.get("healthcheck"), name + " needs a health check"
    volumes = services["postgres"]["volumes"]
    assert len(volumes) == 1
    assert volumes[0]["type"] == "volume" and volumes[0]["source"] == "postgres-data"
    assert volumes[0]["target"] == "/var/lib/postgresql", "Postgres 18 volume layout"
    assert "postgres-data" in config["volumes"]
    core = services["arknet-core"]
    assert core["environment"]["ARKNET_CORE_ADDR"] == ":8080"
    assert core["environment"]["PGHOST"] == "postgres"
    assert core["environment"]["PGPORT"] == "5432"
    assert core["environment"]["PGPASSWORD"] == services["postgres"]["environment"]["POSTGRES_PASSWORD"]
    assert core["depends_on"]["postgres"]["condition"] == "service_healthy"
    ports = core["ports"]
    assert len(ports) == 1 and ports[0]["host_ip"] == "127.0.0.1"
    assert ports[0]["target"] == 8080
    for name in ["Dockerfile", ".dockerignore"]:
        assert (ROOT / "core" / name).is_file()
    assert "POSTGRES_PASSWORD=\n" in (DEPLOY / ".env.example").read_text()
    for password in [None, ""]:
        without_password = dict(os.environ)
        without_password.pop("POSTGRES_PASSWORD", None)
        if password is not None:
            without_password["POSTGRES_PASSWORD"] = password
        rejected = subprocess.run(
            ["docker", "compose", "--env-file", os.devnull, "config", "--quiet"],
            cwd=DEPLOY, env=without_password, capture_output=True, text=True,
        )
        assert rejected.returncode != 0 and "POSTGRES_PASSWORD" in rejected.stderr
    print("PASS: two services, private PostgreSQL, named storage, health checks and DB-health startup ordering")
    print("PASS: startup configuration rejects an unset or empty database password")


if __name__ == "__main__":
    main()
