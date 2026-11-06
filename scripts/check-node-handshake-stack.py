#!/usr/bin/env python3
"""Verify approval handshake and credential isolation with two disposable Nodes.

Generated secrets stay in environment/in-memory; no developer .env is modified.
Only uniquely named test resources are removed. No UI containers are needed.
"""
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from scripts.core_admin import ScopedAdmin  # noqa: E402


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return str(sock.getsockname()[1])


def main():
    prefix = "arknet-handshake-test-" + uuid.uuid4().hex[:12]
    network = prefix + "-transport"
    admin = None  # scoped token minted after Core starts
    core_port = free_port()
    kinds = ["core", "a", "b"]
    projects = {kind: prefix + "-" + kind for kind in kinds}
    images = {component: prefix + "-" + component + "-tests" for component in ["core", "node-agent"]}
    envs = {}
    for kind in kinds:
        envs[kind] = dict(os.environ, POSTGRES_USER="arknet", POSTGRES_DB="arknet_core" if kind == "core" else "arknet_node",
                          POSTGRES_PASSWORD=secrets.token_hex(32), PGSSLMODE="disable", ARKNET_CORE_PORT=core_port,
                          ARKNET_CORE_URL="", ARKNET_CORE_ALLOW_HTTP="false", ARKNET_AGENT_PORT=free_port(), ARKNET_UI_PORT=free_port())

    def docker(*args, capture=True, environment=None):
        result = subprocess.run(["docker", *args], check=True, text=True, capture_output=capture, env=environment)
        return result.stdout.strip() if capture else None

    def status(kind):
        with urllib.request.urlopen("http://127.0.0.1:" + envs[kind]["ARKNET_AGENT_PORT"] + "/api/v1/status", timeout=3) as response:
            return json.load(response)

    def wait_state(kind, expected):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            try:
                found = status(kind)
                if found["state"] == expected:
                    return found
            except (OSError, ValueError):
                pass
            time.sleep(0.25)
        raise AssertionError("Node " + kind + " did not reach " + expected)

    def approve(node_id):
        req = urllib.request.Request("http://127.0.0.1:" + core_port + "/api/admin/v1/nodes/" + node_id + "/approve",
                                     headers=admin.headers(), method="POST")
        with urllib.request.urlopen(req, timeout=10) as response:
            assert response.status == 200
            assert json.load(response) == {"node_id": node_id, "enrollment_status": "APPROVED", "trust_status": "TRUSTED"}

    with tempfile.TemporaryDirectory(prefix=prefix) as directory:
        overrides = {}
        for kind in kinds:
            local = "core" if kind == "core" else "node"
            service = "arknet-core" if kind == "core" else "arknet-agent"
            override = {"services": {service: {"networks": [local, "handshake-test"]}},
                        "networks": {"handshake-test": {"external": True, "name": network}}}
            if kind != "core":
                override["services"][service]["environment"] = {"ARKNET_CORE_URL": "http://arknet-core:8080", "ARKNET_CORE_ALLOW_HTTP": "true"}
            path = Path(directory) / (kind + ".json")
            path.write_text(json.dumps(override))
            overrides[kind] = path

        def compose(kind, *args, capture=False, check=True):
            component = "core" if kind == "core" else "node"
            result = subprocess.run(["docker", "compose", "-p", projects[kind], "-f", str(ROOT / "deploy" / component / "compose.yaml"),
                                     "-f", str(overrides[kind]), *args], cwd=ROOT / "deploy" / component,
                                    env=envs[kind], check=check, text=True, capture_output=capture)
            return result.stdout.strip() if capture else None

        admin = ScopedAdmin(lambda *a, **k: compose("core", *a, **k), "handshake-stack", "nodes.decide")

        def sql(kind, statement):
            return compose(kind, "exec", "-T", "postgres", "psql", "-U", "arknet", "-d", envs[kind]["POSTGRES_DB"],
                           "-At", "-v", "ON_ERROR_STOP=1", "-c", statement, capture=True)

        def db_env(kind):
            e = dict(envs[kind])
            e["ARKNET_TEST_DATABASE_URL"] = "postgresql://arknet:" + e["POSTGRES_PASSWORD"] + "@postgres:5432/" + e["POSTGRES_DB"] + "?sslmode=disable"
            return e

        def probe(kind, target=""):
            e = db_env(kind)
            e.update(ARKNET_TEST_CORE_URL="http://host.docker.internal:" + core_port, ARKNET_TEST_TARGET_NODE_ID=target)
            docker("run", "--rm", "--network", projects[kind] + "_node", "-e", "ARKNET_TEST_DATABASE_URL",
                   "-e", "ARKNET_TEST_CORE_URL", "-e", "ARKNET_TEST_TARGET_NODE_ID", "-e", "CGO_ENABLED=0", "-e", "GOMAXPROCS=2",
                   "-v", str(ROOT / "scripts/node-enrollment-auth-probe.go") + ":/src/probe/main.go:ro",
                   images["node-agent"], "go", "run", "-trimpath", "-p=1", "./probe", capture=False, environment=e)

        created_network = False
        try:
            docker("network", "create", network)
            created_network = True
            compose("core", "up", "--build", "-d", "--wait", "--wait-timeout", "120")
            pending = {}
            for kind in ["a", "b"]:
                compose(kind, "up", "--build", "-d", "--wait", "--wait-timeout", "120", "postgres", "arknet-agent")
                pending[kind] = wait_state(kind, "ENROLLMENT_PENDING")
            ids = {kind: pending[kind]["node_id"] for kind in pending}
            assert ids["a"] != ids["b"]
            assert sql("core", "SELECT count(*) FROM nodes") == "2"
            assert sql("core", "SELECT count(DISTINCT enrollment_public_key) FROM nodes WHERE enrollment_public_key IS NOT NULL") == "2"
            print("PASS: two fresh nodes persist distinct credentials and Core pins distinct public keys", flush=True)

            docker("build", "--target", "build", "-t", images["node-agent"], str(ROOT / "node-agent"))
            probe("a")
            probe("a", ids["b"])
            probe("b", ids["a"])
            print("PASS: A credential cannot authenticate B; B credential cannot authenticate A", flush=True)

            connected = {}
            for kind in ["a", "b"]:
                approve(ids[kind])
                connected[kind] = wait_state(kind, "CONNECTED")
                assert connected[kind]["node_id"] == ids[kind]
                assert sql(kind, "SELECT status='APPROVED' AND core_url='http://arknet-core:8080' AND enrolled_at IS NOT NULL FROM core_enrollment") == "t"
                assert sql(kind, "SELECT count(*) FROM node_state_transitions") == "2"
                assert sql(kind, "SELECT count(*) FROM node_state_transitions WHERE from_state='ENROLLMENT_PENDING' AND to_state='CONNECTED'") == "1"
            probe("a")
            probe("a", ids["b"])
            print("PASS: running approved nodes learn approval outbound and atomically reach CONNECTED", flush=True)

            # Snapshot metadata only. Never select or print private seed material.
            persisted = {kind: sql(kind, "SELECT row_to_json(e) FROM core_enrollment e") for kind in ["a", "b"]}
            pinned = sql("core", "SELECT public_id || ':' || encode(enrollment_public_key,'hex') FROM nodes ORDER BY public_id")
            compose("core", "stop", "arknet-core")
            for kind in ["a", "b"]:
                # Restart PostgreSQL first and wait for readiness. Restarting the
                # Agent concurrently races its one-shot startup DB connection.
                compose(kind, "restart", "postgres")
                compose(kind, "up", "-d", "--wait", "--wait-timeout", "120", "postgres")
                compose(kind, "restart", "arknet-agent")
                compose(kind, "up", "-d", "--wait", "--wait-timeout", "120", "arknet-agent")
                # Connection state may degrade; completed enrollment must persist.
                assert status(kind)["node_id"] == ids[kind]
                assert status(kind)["state"] in {"CONNECTED", "DEGRADED", "OFFLINE", "RECONNECTING"}
                assert sql(kind, "SELECT row_to_json(e) FROM core_enrollment e") == persisted[kind]
                assert sql(kind, "SELECT count(*) FROM node_state_transitions WHERE from_state='ENROLLMENT_PENDING' AND to_state='CONNECTED'") == "1"
                compose(kind, "up", "-d", "--no-deps", "--force-recreate", "arknet-agent", "--wait", "--wait-timeout", "120")
                assert status(kind)["node_id"] == ids[kind]
                assert sql(kind, "SELECT row_to_json(e) FROM core_enrollment e") == persisted[kind]
            print("PASS: Node/DB restart and recreation preserve completed enrollment with Core offline", flush=True)
            compose("core", "start", "arknet-core")
            compose("core", "up", "-d", "--wait", "--wait-timeout", "120")
            assert sql("core", "SELECT count(*) FROM nodes") == "2"
            assert sql("core", "SELECT public_id || ':' || encode(enrollment_public_key,'hex') FROM nodes ORDER BY public_id") == pinned
            probe("a")
            probe("b")
            probe("a", ids["b"])
            print("PASS: saved credentials remain valid for their own nodes after reboot/recreation", flush=True)

            for component, kind, local in [("core", "core", "core"), ("node-agent", "a", "node")]:
                docker("build", "--target", "build", "-t", images[component], str(ROOT / component))
                docker("run", "--rm", "--network", projects[kind] + "_" + local, "-e", "ARKNET_TEST_DATABASE_URL",
                       "-e", "CGO_ENABLED=0", "-e", "GOMAXPROCS=2", "-v", str(ROOT / "VERSION") + ":/VERSION:ro",
                       images[component], "go", "test", "-trimpath", "-p=1", "-count=1", "./...", capture=False, environment=db_env(kind))
            assert sql("core", "SELECT count(*) FROM nodes") == "2"
            for kind in ["a", "b"]:
                assert wait_state(kind, "CONNECTED")["node_id"] == ids[kind]
                logs = compose(kind, "logs", "arknet-agent", capture=True)
                assert not any(t in logs for t in admin.secrets())
                assert "private_seed" not in json.dumps(status(kind))
            print("PASS: real PostgreSQL migration/persistence/handshake tests; secrets absent from public status", flush=True)
        except Exception:
            for kind in kinds:
                compose(kind, "logs", "--tail", "20", check=False)
            raise
        finally:
            errors = []
            for kind in ["b", "a", "core"]:
                try:
                    compose(kind, "down", "--volumes", "--remove-orphans")
                except Exception as error:
                    errors.append(error)
            for image in images.values():
                if subprocess.run(["docker", "image", "inspect", image], capture_output=True).returncode == 0:
                    docker("image", "rm", image)
            if created_network:
                docker("network", "rm", network)
            if errors:
                raise RuntimeError("Disposable handshake cleanup failed") from errors[0]
            print("Removed only disposable handshake resources: " + prefix, flush=True)


if __name__ == "__main__":
    main()
