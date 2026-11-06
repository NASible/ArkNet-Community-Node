#!/usr/bin/env python3
"""Verify outbound enrollment using disposable Core and Node Compose projects.

No developer .env or database is changed. Generated passwords stay in environment.
Only test-specific resources are removed. Docker Compose, Python 3 and OpenSSL 3 required.
"""
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from scripts.enrollment_signing import EnrollmentSigner, temporary_root  # noqa: E402
from scripts import operator_login  # noqa: E402


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return str(sock.getsockname()[1])


def main():
    prefix = "arknet-enrollment-test-" + uuid.uuid4().hex[:12]
    network = prefix + "-transport"
    env = dict(os.environ, POSTGRES_USER="arknet", PGSSLMODE="disable",
               ARKNET_CORE_PORT=free_port(), ARKNET_AGENT_PORT=free_port(),
               ARKNET_UI_PORT=free_port(), ARKNET_MEMBER_PORT=free_port(), ARKNET_CORE_URL="",
               ARKNET_CORE_ALLOW_HTTP="false")
    environments = {
        "core": dict(env, POSTGRES_DB="arknet_core", POSTGRES_PASSWORD=secrets.token_hex(32)),
        "node": dict(env, POSTGRES_DB="arknet_node", POSTGRES_PASSWORD=secrets.token_hex(32)),
    }
    projects = {kind: prefix + "-" + kind for kind in environments}
    images = {kind: prefix + "-" + kind + "-tests" for kind in environments}
    endpoint = "http://127.0.0.1:" + env["ARKNET_CORE_PORT"] + "/api/federation/v1/enrollments"

    def docker(*args):
        return subprocess.run(["docker", *args], check=True, capture_output=True, text=True).stdout.strip()

    def fetch_status():
        request = urllib.request.Request("http://127.0.0.1:" + env["ARKNET_AGENT_PORT"] + "/api/v1/status",
                                         headers=operator.headers)
        with urllib.request.urlopen(request, timeout=5) as response:
            return json.load(response)

    def request(url, method="GET", body=None, headers=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(url, data=data, method=method,
                                     headers={"Content-Type": "application/json", **(headers or {})})
        try:
            with urllib.request.urlopen(req, timeout=5) as response:
                return response.status, dict(response.headers), response.read()
        except urllib.error.HTTPError as error:
            return error.code, dict(error.headers), error.read()

    with tempfile.TemporaryDirectory(prefix=prefix, dir=temporary_root()) as directory, EnrollmentSigner() as signer:
        overrides = {}
        for kind, service, local_network in [("core", "arknet-core", "core"), ("node", "arknet-agent", "node")]:
            override = {"services": {service: {"networks": [local_network, "enrollment-test"]}},
                        "networks": {"enrollment-test": {"external": True, "name": network}}}
            if kind == "node":
                override["services"][service]["environment"] = {
                    "ARKNET_CORE_URL": "http://arknet-core:8080", "ARKNET_CORE_ALLOW_HTTP": "true"}
            path = Path(directory) / (kind + ".json")
            path.write_text(json.dumps(override))
            overrides[kind] = path

        def compose(kind, *args, capture=False, check=True):
            base = ["docker", "compose", "-p", projects[kind], "-f", str(ROOT / "deploy" / kind / "compose.yaml"),
                    "-f", str(overrides[kind])]
            result = subprocess.run(base + list(args), cwd=ROOT / "deploy" / kind,
                                    env=environments[kind], check=check, text=True, capture_output=capture)
            return result.stdout.strip() if capture else None

        def sql(kind, statement):
            return compose(kind, "exec", "-T", "postgres", "psql", "-U", "arknet", "-d",
                           environments[kind]["POSTGRES_DB"], "-At", "-v", "ON_ERROR_STOP=1", "-c", statement, capture=True)

        def run_repository_tests(kind, component, local_network):
            docker("build", "--target", "build", "-t", images[kind], str(ROOT / component))
            test_env = dict(environments[kind])
            test_env["ARKNET_TEST_DATABASE_URL"] = "postgresql://arknet:" + test_env["POSTGRES_PASSWORD"] + "@postgres:5432/" + test_env["POSTGRES_DB"] + "?sslmode=disable"
            subprocess.run(["docker", "run", "--rm", "-e", "GOMAXPROCS=2", "-e", "CGO_ENABLED=0", "--network", projects[kind] + "_" + local_network,
                            "-e", "ARKNET_TEST_DATABASE_URL", images[kind], "go", "test", "-trimpath", "-p=1", "-count=1", "./internal/..."],
                           env=test_env, check=True)

        created_network = False
        try:
            docker("network", "create", network)
            created_network = True
            compose("core", "up", "--build", "-d", "--wait", "--wait-timeout", "120")
            assert sql("core", "SELECT count(*) FROM nodes") == "0"
            compose("node", "up", "--build", "-d", "--wait", "--wait-timeout", "120", "postgres", "arknet-agent")
            code = operator_login.setup_code(compose("node", "exec", "-T", "arknet-agent", "arknet-agent",
                                                     "operator", "setup-code", capture=True))
            operator = operator_login.bootstrap("http://127.0.0.1:" + env["ARKNET_AGENT_PORT"], code, request,
                                                member_base="http://127.0.0.1:" + env["ARKNET_MEMBER_PORT"])
            initial = fetch_status()
            assert initial["state"] == "ENROLLMENT_PENDING", initial
            node_id = initial["node_id"]
            assert sql("core", "SELECT count(*) FROM nodes") == "1"
            row = json.loads(sql("core", "SELECT row_to_json(n) FROM nodes n"))
            assert row["public_id"] == node_id and row["software_version"] == initial["version"]
            assert row["enrollment_status"] == "PENDING" and row["trust_status"] == "UNTRUSTED"
            assert row["last_seen_at"] is None
            assert sql("node", "SELECT count(*) FROM node_state_transitions") == "1"
            assert sql("node", "SELECT status = 'PENDING' AND core_url = 'http://arknet-core:8080' AND enrolled_at IS NULL FROM core_enrollment") == "t"
            print("PASS: fresh Agent enrolled outbound; Core has exactly one untrusted pending node", flush=True)

            payload = {"node_id": node_id, "version": initial["version"], "capabilities": {}}

            def post(body):
                extra = {key: value for key, value in body.items()
                         if key not in {"node_id", "version", "capabilities", "public_key"}}
                request = signer.request(endpoint, body["node_id"], body["version"], extra=extra)
                with urllib.request.urlopen(request, timeout=10) as response:
                    assert response.status == 200
                    assert json.load(response) == {"node_id": node_id, "enrollment_status": "PENDING"}

            # This probe deliberately has a different key from the real Agent.
            # Valid conflicting-key proofs get the same generic receipt and must
            # not change the Agent's pinned identity or metadata. Pace actual
            # attempts below the 5-burst/10-per-minute peer limit (also global).
            for _ in range(16):
                post(payload)
            assert sql("core", "SELECT count(*) FROM nodes") == "1"
            assert json.loads(sql("core", "SELECT row_to_json(n) FROM nodes n")) == row
            print("PASS: paced signed conflicting-key retries return generic receipts without duplicates or metadata changes", flush=True)

            for forbidden in ["wan_ip", "mac_address", "hardware_serial", "operator_hostname", "filesystem"]:
                bad = dict(payload, **{forbidden: "must-not-be-stored"})
                try:
                    post(bad)
                    raise AssertionError("Core accepted forbidden metadata")
                except urllib.error.HTTPError as response:
                    assert response.code == 400
            assert sql("core", "SELECT count(*) FROM nodes") == "1"
            print("PASS: Core rejects extra identifying metadata fields", flush=True)

            for recreate in [False, True]:
                if recreate:
                    old_agent = compose("node", "ps", "-q", "arknet-agent", capture=True)
                    old_db = compose("node", "ps", "-q", "postgres", capture=True)
                    compose("node", "up", "-d", "--no-deps", "--force-recreate", "arknet-agent", "--wait", "--wait-timeout", "120")
                    assert compose("node", "ps", "-q", "arknet-agent", capture=True) != old_agent
                    assert compose("node", "ps", "-q", "postgres", capture=True) == old_db
                else:
                    compose("node", "restart", "arknet-agent")
                    compose("node", "up", "-d", "--wait", "--wait-timeout", "120", "postgres", "arknet-agent")
                assert fetch_status() == initial
                assert sql("node", "SELECT count(*) FROM node_state_transitions") == "1"
                assert sql("core", "SELECT count(*) FROM nodes") == "1"
                assert json.loads(sql("core", "SELECT row_to_json(n) FROM nodes n")) == row
            print("PASS: Agent restart/recreation retries preserve identity, state timestamp and one enrollment record", flush=True)

            run_repository_tests("core", "core", "core")
            run_repository_tests("node", "node-agent", "node")
            assert sql("core", "SELECT count(*) FROM nodes") == "1"
            assert json.loads(sql("core", "SELECT row_to_json(n) FROM nodes n")) == row
            assert fetch_status() == initial
            assert sql("node", "SELECT count(*) FROM node_state_transitions") == "1"
            print("PASS: real PostgreSQL Core and Agent enrollment repository tests remain schema-isolated", flush=True)
        except Exception:
            for kind in environments:
                compose(kind, "logs", "--tail", "30", check=False)
            raise
        finally:
            # Each cleanup targets only this test's uniquely named resources.
            cleanup_errors = []
            for kind in ["node", "core"]:
                try:
                    compose(kind, "down", "--volumes", "--remove-orphans")
                except Exception as error:
                    cleanup_errors.append(error)
                if subprocess.run(["docker", "image", "inspect", images[kind]], capture_output=True).returncode == 0:
                    docker("image", "rm", images[kind])
            if created_network:
                docker("network", "rm", network)
            if cleanup_errors:
                raise RuntimeError("Disposable test cleanup failed") from cleanup_errors[0]
            print("Removed only disposable enrollment projects, transport network, volumes and helper images: " + prefix, flush=True)


if __name__ == "__main__":
    main()
