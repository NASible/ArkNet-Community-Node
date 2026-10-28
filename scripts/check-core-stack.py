#!/usr/bin/env python3
"""Exercise an isolated disposable Core stack, never an existing project.

Requires Docker Compose and Python 3. No .env file or password is written.
Only the unique test project's containers, network, and volume are removed.
"""
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import urllib.request
import urllib.error
import time
import uuid

ROOT = Path(__file__).resolve().parent.parent
DEPLOY = ROOT / "deploy" / "core"


def main():
    project = "arknet-core-test-" + uuid.uuid4().hex[:12]
    test_image = project + "-tests"
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = str(sock.getsockname()[1])
    env = dict(os.environ, ARKNET_CORE_DEV_ADMIN_ENABLED="false", ARKNET_CORE_DEV_ADMIN_TOKEN="",
               POSTGRES_PASSWORD=secrets.token_hex(32),
               POSTGRES_USER="arknet", POSTGRES_DB="arknet_core",
               ARKNET_CORE_PORT=port, ARKNET_CORE_LOG_LEVEL="INFO", PGSSLMODE="disable")
    base = ["docker", "compose", "-p", project]

    def compose(*args, capture=False):
        result = subprocess.run(base + list(args), cwd=DEPLOY, env=env,
                                check=True, text=True, capture_output=capture)
        return result.stdout.strip() if capture else None

    def docker(*args):
        return subprocess.run(["docker", *args], env=env, check=True,
                              capture_output=True, text=True).stdout.strip()

    def sql(statement):
        return compose("exec", "-T", "postgres", "psql", "-U", "arknet",
                       "-d", "arknet_core", "-At", "-c", statement, capture=True)

    def check_http():
        for path, expected in [("/health", {"healthy": True, "database_healthy": True}),
                               ("/api/federation/v1/version", {"version": "0.0.1-dev"})]:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=5) as response:
                assert response.status == 200
                assert response.headers.get_content_type() == "application/json"
                assert json.load(response) == expected
        print("PASS: live Core health and version endpoints return valid JSON", flush=True)

    try:
        compose("up", "--build", "-d", "--wait", "--wait-timeout", "120")
        ids = {name: compose("ps", "-q", name, capture=True)
               for name in ["postgres", "arknet-core"]}
        assert all(ids.values()), "Both containers must start"
        for name, container in ids.items():
            info = json.loads(docker("inspect", container))[0]
            assert info["State"]["Health"]["Status"] == "healthy", name
        print("PASS: Core and PostgreSQL both healthy", flush=True)
        check_http()
        migration_count = len(list((ROOT / "core" / "internal" / "postgres" / "migrations").glob("*.sql")))
        assert sql("SELECT count(*) FROM arknet_schema_migrations") == str(migration_count)
        assert sql("SELECT to_regclass('nodes')::text") == "nodes"
        assert sql("SELECT to_regclass('media_objects')::text") == "media_objects"
        print("PASS: fresh database migrated automatically", flush=True)
        docker("build", "--target", "build", "-t", test_image, str(ROOT / "core"))
        env["ARKNET_TEST_DATABASE_URL"] = "postgresql://arknet:" + env["POSTGRES_PASSWORD"] + "@postgres:5432/arknet_core?sslmode=disable"
        subprocess.run(["docker", "run", "--rm", "-e", "GOMAXPROCS=2", "-e", "CGO_ENABLED=0", "--network", project + "_core",
                        "-e", "ARKNET_TEST_DATABASE_URL", "-v", str(ROOT / "VERSION") + ":/VERSION:ro", test_image,
                        "go", "test", "-trimpath", "-p=1", "-count=1", "-v", "./..."], env=env, check=True)
        print("PASS: real PostgreSQL node/media CRUD and migration tests", flush=True)

        postgres = json.loads(docker("inspect", ids["postgres"]))[0]
        assert not postgres["HostConfig"]["PortBindings"]
        assert all(value is None for value in postgres["NetworkSettings"]["Ports"].values())
        assert any(m["Type"] == "volume" and m["Destination"] == "/var/lib/postgresql"
                   for m in postgres["Mounts"])
        print("PASS: PostgreSQL has no host ports and uses a named volume", flush=True)

        # Share Core's network namespace for an authenticated connectivity probe,
        # without adding a DB client or application integration to the Core image.
        env.update(PGHOST="postgres", PGPORT="5432", PGUSER="arknet",
                   PGDATABASE="arknet_core", PGPASSWORD=env["POSTGRES_PASSWORD"])
        result = docker("run", "--rm", "--network", "container:" + ids["arknet-core"],
                        "-e", "PGHOST", "-e", "PGPORT", "-e", "PGUSER", "-e", "PGDATABASE",
                        "-e", "PGPASSWORD", "postgres:18-alpine", "psql", "-At", "-c", "SELECT 1")
        assert result == "1"
        print("PASS: authenticated PostgreSQL access from Core network namespace", flush=True)

        compose("stop", "postgres")
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=5)
            raise AssertionError("Core claimed healthy with PostgreSQL stopped")
        except urllib.error.HTTPError as response:
            assert response.code == 503
            assert json.load(response) == {"healthy": False, "database_healthy": False}
        print("PASS: Core reports HTTP 503 when PostgreSQL is unavailable", flush=True)
        compose("up", "-d", "postgres", "--wait", "--wait-timeout", "120")
        deadline = time.monotonic() + 10
        while True:
            try:
                check_http()
                break
            except urllib.error.HTTPError:
                if time.monotonic() >= deadline: raise
                time.sleep(0.2)
        print("PASS: Core health recovers without restarting Core", flush=True)

        sql("CREATE TABLE arknet_dev_persistence_probe (value text NOT NULL); "
            "INSERT INTO arknet_dev_persistence_probe VALUES ('retained');")
        compose("restart")
        compose("up", "-d", "--wait", "--wait-timeout", "120")
        assert sql("SELECT value FROM arknet_dev_persistence_probe") == "retained"
        print("PASS: database row survives container restart", flush=True)
        check_http()
        compose("down")
        compose("up", "-d", "--wait", "--wait-timeout", "120")
        assert sql("SELECT value FROM arknet_dev_persistence_probe") == "retained"
        print("PASS: database row survives down/up container recreation", flush=True)
        check_http()
    except Exception:
        subprocess.run(base + ["logs", "--tail", "50"], cwd=DEPLOY, env=env)
        raise
    finally:
        # Destructive cleanup is scoped to this unique disposable test project.
        compose("down", "--volumes", "--remove-orphans")
        if subprocess.run(["docker", "image", "inspect", test_image], capture_output=True).returncode == 0:
            docker("image", "rm", test_image)
        print("Removed only disposable test project and test image: " + project, flush=True)


if __name__ == "__main__":
    main()
