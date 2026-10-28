#!/usr/bin/env python3
"""Exercise administrative enrollment decisions on a disposable Core/PG18 stack.

No developer .env or database is changed; generated credentials stay in environment.
Only this uniquely named project's resources are removed.
"""
import concurrent.futures
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent


def main():
    project = "arknet-admin-test-" + uuid.uuid4().hex[:12]
    image = project + "-go-tests"
    token = secrets.token_hex(32)
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = str(sock.getsockname()[1])
    env = dict(os.environ, POSTGRES_DB="arknet_core", POSTGRES_USER="arknet",
               POSTGRES_PASSWORD=secrets.token_hex(32), ARKNET_CORE_PORT=port,
               PGSSLMODE="disable", ARKNET_CORE_DEV_ADMIN_ENABLED="true",
               ARKNET_CORE_DEV_ADMIN_TOKEN=token)
    base = "http://127.0.0.1:" + port

    def compose(*args, capture=False, check=True, environment=None):
        result = subprocess.run(["docker", "compose", "-p", project, "-f",
                                 str(ROOT / "deploy/core/compose.yaml"), *args],
                                cwd=ROOT / "deploy/core", env=environment or env,
                                check=check, text=True, capture_output=capture)
        return result.stdout.strip() if capture else None

    def request(method, path, body=None, credential=token, status=200):
        headers = {}
        if credential is not None:
            headers["Authorization"] = "Bearer " + credential
        data = None if body is None else json.dumps(body).encode()
        if data is not None:
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(base + path, data=data, headers=headers, method=method)
        try:
            response = urllib.request.urlopen(req, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            assert response.code == status, (path.split("?")[0], response.code, status)
            raw = response.read()
            assert token.encode() not in raw
            if path.startswith("/api/admin/"):
                assert response.headers.get("Cache-Control") == "no-store"
            return json.loads(raw) if response.headers.get_content_type() == "application/json" else None

    def sql(statement):
        return compose("exec", "-T", "postgres", "psql", "-U", "arknet", "-d", "arknet_core",
                       "-At", "-v", "ON_ERROR_STOP=1", "-c", statement, capture=True)

    def row(node_id):
        # IDs are minted by this test, not arbitrary SQL input.
        return json.loads(sql("SELECT row_to_json(n) FROM nodes n WHERE public_id='" + node_id + "'"))

    admin = "/api/admin/v1/nodes/"
    enrollment = "/api/federation/v1/enrollments"
    try:
        compose("up", "--build", "-d", "--wait", "--wait-timeout", "120")
        request("GET", admin + "pending", credential=None, status=401)
        request("GET", admin + "pending", credential=secrets.token_hex(32), status=401)
        request("GET", admin + "pending?token=" + token, credential=None, status=401)
        assert request("GET", admin + "pending") == {"nodes": [], "next_cursor": ""}
        assert sql("SELECT count(*) FROM nodes") == "0"
        ids = sorted("node_" + secrets.token_hex(32) for _ in range(3))
        bodies = {id: {"node_id": id, "version": "0.0.1-dev", "capabilities": {}} for id in ids}
        for body in bodies.values():
            request("POST", enrollment, body, credential=None)
        before = {id: row(id) for id in ids}
        found = []
        cursor = ""
        while True:
            page = request("GET", admin + "pending?limit=1" + ("&cursor=" + cursor if cursor else ""))
            assert len(page["nodes"]) <= 1
            found.extend(n["node_id"] for n in page["nodes"])
            cursor = page["next_cursor"]
            if not cursor:
                break
            assert len(found) <= len(ids)
        assert found == ids
        print("PASS: header-only administrative authorization and complete pending pagination", flush=True)

        for id, action, enrollment_status, trust_status in [
                (ids[0], "approve", "APPROVED", "TRUSTED"),
                (ids[1], "reject", "REJECTED", "BLOCKED")]:
            request("POST", admin + id + "/" + action, credential=None, status=401)
            assert row(id) == before[id]
            result = request("POST", admin + id + "/" + action)
            assert result == {"node_id": id, "enrollment_status": enrollment_status, "trust_status": trust_status}
            expected = dict(before[id], enrollment_status=enrollment_status, trust_status=trust_status)
            assert row(id) == expected
            for duplicate in ["approve", "reject"]:
                request("POST", admin + id + "/" + duplicate, status=409)
            request("POST", enrollment, bodies[id], credential=None, status=409)
            assert row(id) == expected
        assert [n["node_id"] for n in request("GET", admin + "pending")["nodes"]] == [ids[2]]
        print("PASS: approval updates trust; rejection persists; repeats/opposite actions/re-enrollment cannot overwrite decisions", flush=True)

        mixed_id = ids[2]
        def decide(action):
            req = urllib.request.Request(base + admin + mixed_id + "/" + action,
                                         headers={"Authorization": "Bearer " + token}, method="POST")
            try:
                response = urllib.request.urlopen(req, timeout=10)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                return response.code
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            outcomes = list(pool.map(decide, ["approve", "reject"] * 8))
        assert outcomes.count(200) == 1 and outcomes.count(409) == 15
        assert (row(mixed_id)["enrollment_status"], row(mixed_id)["trust_status"]) in [("APPROVED", "TRUSTED"), ("REJECTED", "BLOCKED")]
        assert request("GET", admin + "pending") == {"nodes": [], "next_cursor": ""}
        print("PASS: concurrent competing decisions have exactly one winner", flush=True)

        request("POST", admin + "not-opaque/approve", status=400)
        request("POST", admin + "node_" + secrets.token_hex(32) + "/approve", status=404)
        request("POST", admin + ids[0] + "/reject", {"trust_status": "TRUSTED"}, status=400)
        request("GET", admin + "pending?limit=101", status=400)
        persisted = {id: row(id) for id in ids}
        compose("restart", "arknet-core")
        compose("up", "-d", "--wait", "--wait-timeout", "120")
        assert {id: row(id) for id in ids} == persisted
        request("POST", admin + ids[1] + "/approve", status=409)
        compose("up", "-d", "--no-deps", "--force-recreate", "arknet-core", "--wait", "--wait-timeout", "120")
        assert {id: row(id) for id in ids} == persisted
        assert request("GET", admin + "pending") == {"nodes": [], "next_cursor": ""}
        print("PASS: safe invalid actions and decisions survive Core restart/recreation", flush=True)

        subprocess.run(["docker", "build", "--target", "build", "-t", image, str(ROOT / "core")], check=True)
        test_env = dict(env, ARKNET_TEST_DATABASE_URL="postgresql://arknet:" + env["POSTGRES_PASSWORD"] + "@postgres:5432/arknet_core?sslmode=disable")
        subprocess.run(["docker", "run", "--rm", "--network", project + "_core", "-e", "ARKNET_TEST_DATABASE_URL",
                        "-v", str(ROOT / "VERSION") + ":/VERSION:ro",
                        "-e", "CGO_ENABLED=0", "-e", "GOMAXPROCS=2", image,
                        "go", "test", "-trimpath", "-p=1", "-count=1", "./..."], env=test_env, check=True)
        assert {id: row(id) for id in ids} == persisted
        print("PASS: real PostgreSQL repository/HTTP/config tests; developer and live test schemas remain untouched", flush=True)

        disabled = dict(env, ARKNET_CORE_DEV_ADMIN_ENABLED="false", ARKNET_CORE_DEV_ADMIN_TOKEN="")
        compose("up", "-d", "--no-deps", "--force-recreate", "arknet-core", "--wait", "--wait-timeout", "120", environment=disabled)
        request("GET", admin + "pending", status=404)
        request("GET", "/health", credential=None)
        assert {id: row(id) for id in ids} == persisted
        print("PASS: administrative API disabled without opt-in; public health remains available", flush=True)
    except Exception:
        compose("logs", "--tail", "25", check=False)
        raise
    finally:
        compose("down", "--volumes", "--remove-orphans")
        if subprocess.run(["docker", "image", "inspect", image], capture_output=True).returncode == 0:
            subprocess.run(["docker", "image", "rm", image], check=True, capture_output=True)
        print("Removed only disposable administration test resources: " + project, flush=True)


if __name__ == "__main__":
    main()
