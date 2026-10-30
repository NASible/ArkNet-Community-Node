#!/usr/bin/env python3
"""Live authenticated heartbeat/outage/recovery check on isolated Core and Node.

Never modifies developer .env or databases. Only owns its unique test resources.
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
sys.path.insert(0, str(ROOT))
from scripts import operator_login  # noqa: E402


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return str(s.getsockname()[1])


def main():
    prefix = "arknet-heartbeat-test-" + uuid.uuid4().hex[:12]
    network = prefix + "-transport"
    token = secrets.token_hex(32)
    ui_port = free_port()
    ports = {"core": free_port(), "node": free_port()}
    member_port = free_port()
    projects = {k: prefix + "-" + k for k in ports}
    envs = {k: dict(os.environ, POSTGRES_PASSWORD=secrets.token_hex(32), POSTGRES_USER="arknet",
                   POSTGRES_DB="arknet_core" if k == "core" else "arknet_node", PGSSLMODE="disable",
                   ARKNET_CORE_PORT=ports["core"], ARKNET_AGENT_PORT=ports["node"], ARKNET_UI_PORT=ui_port,
                   ARKNET_MEMBER_PORT=member_port,
                   ARKNET_CORE_URL="", ARKNET_CORE_ALLOW_HTTP="false", ARKNET_HEARTBEAT_INTERVAL="5s",
                   ARKNET_CORE_DEV_ADMIN_ENABLED="true" if k == "core" else "false",
                   ARKNET_CORE_DEV_ADMIN_TOKEN=token if k == "core" else "") for k in ports}
    images = {k: prefix + "-" + k + "-tests" for k in ports}

    # Node management routes need an operator session (45B); /health is on the
    # member listener (45C). `operator` is set once the Node stack is up.
    operator = None

    def http(kind, path, method="GET", admin=False):
        headers = {"Authorization": "Bearer " + token} if admin else {}
        if kind in ("node", "ui"):
            headers.update(operator.headers)
        port = {"ui": ui_port, "member": member_port}.get(kind) or ports[kind]
        req = urllib.request.Request("http://127.0.0.1:" + port + path, headers=headers, method=method)
        with urllib.request.urlopen(req, timeout=3) as r:
            assert r.status == 200
            return json.load(r)

    def request(url, method="GET", body=None, headers=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(url, data=data, method=method,
                                     headers={"Content-Type": "application/json", **(headers or {})})
        try:
            with urllib.request.urlopen(req, timeout=5) as r:
                return r.status, dict(r.headers), r.read()
        except urllib.error.HTTPError as error:
            return error.code, dict(error.headers), error.read()

    def wait(label, fn, timeout=100):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            try:
                value = fn()
                if value:
                    return value
            except (OSError, ValueError):
                pass
            time.sleep(0.25)
        raise AssertionError("Timed out: " + label)

    def status():
        return http("node", "/api/v1/status")

    def state_is(wanted):
        s = status()
        return s if s["state"] == wanted else None

    with tempfile.TemporaryDirectory(prefix=prefix) as tmp:
        files = {}
        for kind in ports:
            service = "arknet-core" if kind == "core" else "arknet-agent"
            override = {"services": {service: {"networks": [kind, "heartbeat-test"]}},
                        "networks": {"heartbeat-test": {"external": True, "name": network}}}
            if kind == "node":
                override["services"][service]["environment"] = {"ARKNET_CORE_URL": "http://arknet-core:8080",
                                                                  "ARKNET_CORE_ALLOW_HTTP": "true",
                                                                  "ARKNET_HEARTBEAT_INTERVAL": "5s"}
            files[kind] = Path(tmp) / (kind + ".json")
            files[kind].write_text(json.dumps(override))

        def compose(kind, *args, capture=False, check=True):
            r = subprocess.run(["docker", "compose", "-p", projects[kind], "-f", str(ROOT / "deploy" / kind / "compose.yaml"),
                                "-f", str(files[kind]), *args], env=envs[kind], cwd=ROOT / "deploy" / kind,
                               check=check, capture_output=capture, text=True)
            return r.stdout.strip() if capture else None

        def sql(kind, query):
            return compose(kind, "exec", "-T", "postgres", "psql", "-U", "arknet", "-d", envs[kind]["POSTGRES_DB"],
                           "-At", "-v", "ON_ERROR_STOP=1", "-c", query, capture=True)

        created = False
        try:
            subprocess.run(["docker", "network", "create", network], check=True, capture_output=True)
            created = True
            compose("core", "up", "--build", "-d", "--wait", "--wait-timeout", "120")
            compose("node", "up", "--build", "-d", "--wait", "--wait-timeout", "120")
            code = operator_login.setup_code(compose("node", "exec", "-T", "arknet-agent", "arknet-agent", "operator",
                                                     "setup-code", capture=True))
            operator = operator_login.bootstrap("http://127.0.0.1:" + ports["node"], code, request,
                                                member_base="http://127.0.0.1:" + member_port)
            pending = wait("enrollment pending", lambda: state_is("ENROLLMENT_PENDING"))
            node_id = pending["node_id"]
            assert pending["enrollment_state"] == "PENDING"
            assert pending["agent_healthy"] is True and pending["database_healthy"] is True
            assert pending["last_successful_core_contact_at"] is None
            detail_path = "/api/admin/v1/nodes/" + node_id
            before = http("core", detail_path, admin=True)
            assert before["last_seen_at"] is None
            assert before["state"] is None
            http("core", detail_path + "/approve", method="POST", admin=True)
            wait("approved CONNECTED", lambda: state_is("CONNECTED"))
            first = wait("first Core heartbeat", lambda: (lambda d: d if d["last_seen_at"] and d["state"] == "CONNECTED" else None)(http("core", detail_path, admin=True)))
            assert first["version"] == "0.0.1-dev"
            second = wait("last-seen advances", lambda: (lambda d: d if d["last_seen_at"] != first["last_seen_at"] else None)(http("core", detail_path, admin=True)))
            assert second["version"] == first["version"]
            connected_status = status()
            assert connected_status["core_connectivity"] == "CONNECTED"
            assert connected_status["enrollment_state"] == "APPROVED"
            assert connected_status["agent_healthy"] is True and connected_status["database_healthy"] is True
            assert connected_status["jellyfin_healthy"] is True
            assert connected_status["last_successful_core_contact_at"] is not None
            assert set(connected_status) == {"node_id", "version", "state", "state_changed_at",
                                              "enrollment_state", "core_connectivity",
                                              "last_successful_core_contact_at", "database_healthy",
                                              "agent_healthy", "jellyfin_healthy", "release"}
            serialized_status = json.dumps(connected_status).lower()
            for prohibited in ("private", "seed", "credential", "core_url", "database_url", "token"):
                assert prohibited not in serialized_status
            assert http("member", "/health")["healthy"] is True
            ui_connected = http("ui", "/api/v1/status")
            for key in ("node_id", "state", "enrollment_state", "core_connectivity",
                        "database_healthy", "agent_healthy"):
                assert ui_connected[key] == connected_status[key]
            assert ui_connected["last_successful_core_contact_at"] is not None
            print("PASS: authenticated Core detail displays fresh server last-seen, CONNECTED and reported version", flush=True)

            # Exercise the full recovery path from a real MESH state through the
            # Agent's state-machine boundary rather than mutating PostgreSQL directly.
            subprocess.run(["docker", "build", "--target", "build", "-t", images["node"], str(ROOT / "node-agent")], check=True)
            probe_env = dict(envs["node"],
                             ARKNET_TEST_DATABASE_URL="postgresql://arknet:" + envs["node"]["POSTGRES_PASSWORD"] + "@postgres:5432/" + envs["node"]["POSTGRES_DB"] + "?sslmode=disable")
            for next_state in ("SYNCHRONIZING", "MESH"):
                probe_env["ARKNET_TEST_NEXT_STATE"] = next_state
                subprocess.run(["docker", "run", "--rm", "--network", projects["node"] + "_node",
                                "-e", "ARKNET_TEST_DATABASE_URL", "-e", "ARKNET_TEST_NEXT_STATE",
                                "-e", "GOMAXPROCS=2", "-e", "CGO_ENABLED=0",
                                "-v", str(ROOT / "scripts" / "node-state-probe.go") + ":/src/probe/main.go:ro",
                                images["node"], "go", "run", "-trimpath", "-p=1", "./probe"],
                               env=probe_env, check=True)
            wait("Core sees MESH", lambda: (lambda d: d if d["state"] == "MESH" else None)(http("core", detail_path, admin=True)))
            assert http("ui", "/api/v1/status")["state"] == "MESH"

            enrolled = sql("node", "SELECT row_to_json(e) FROM (SELECT status,core_url,enrolled_at FROM core_enrollment) e")
            pinned = sql("core", "SELECT encode(enrollment_public_key,'hex') FROM nodes")
            last_contact = status()["last_successful_core_contact_at"]
            assert last_contact is not None
            compose("core", "stop", "arknet-core")
            degraded = wait("DEGRADED on temporary Core failure", lambda: state_is("DEGRADED"), timeout=30)
            assert degraded["node_id"] == node_id
            assert http("member", "/health")["healthy"] is True
            offline = wait("OFFLINE after repeated failed attempts", lambda: state_is("OFFLINE"), timeout=100)
            assert offline["agent_healthy"] is True and offline["database_healthy"] is True
            assert offline["jellyfin_healthy"] is True
            assert offline["core_connectivity"] == "OFFLINE"
            assert offline["enrollment_state"] == "APPROVED"
            assert offline["last_successful_core_contact_at"] == last_contact
            assert http("ui", "/api/v1/status") == offline
            assert sql("node", "SELECT row_to_json(e) FROM (SELECT status,core_url,enrolled_at FROM core_enrollment) e") == enrolled
            assert http("member", "/health")["healthy"] is True
            assert sql("node", "SELECT count(*) FROM node_state_transitions WHERE to_state='DEGRADED'") != "0"
            print("PASS: Core outage persists DEGRADED then OFFLINE; Agent API and local DB remain healthy", flush=True)

            compose("core", "start", "arknet-core")
            compose("core", "up", "-d", "--wait", "--wait-timeout", "120")
            reconnected_status = wait("automatic reconnect", lambda: state_is("CONNECTED"), timeout=100)
            assert reconnected_status["last_successful_core_contact_at"] != last_contact
            assert reconnected_status["enrollment_state"] == "APPROVED"
            assert reconnected_status["agent_healthy"] is True and reconnected_status["database_healthy"] is True
            assert reconnected_status["jellyfin_healthy"] is True
            ui_reconnected = http("ui", "/api/v1/status")
            for key in ("node_id", "state", "enrollment_state", "core_connectivity",
                        "database_healthy", "agent_healthy"):
                assert ui_reconnected[key] == reconnected_status[key]
            assert ui_reconnected["last_successful_core_contact_at"] != last_contact
            recovered = wait("Core sees recovered CONNECTED", lambda: (lambda d: d if d["state"] == "CONNECTED" and d["last_seen_at"] != second["last_seen_at"] else None)(http("core", detail_path, admin=True)))
            assert recovered["version"] == "0.0.1-dev"
            assert sql("node", "SELECT row_to_json(e) FROM (SELECT status,core_url,enrolled_at FROM core_enrollment) e") == enrolled
            assert sql("core", "SELECT encode(enrollment_public_key,'hex') FROM nodes") == pinned
            assert sql("core", "SELECT count(*) FROM nodes") == "1"
            assert sql("node", "SELECT count(*) FROM node_state_transitions WHERE from_state='ENROLLMENT_PENDING' AND to_state='CONNECTED'") == "1"
            assert sql("node", "SELECT count(*) FROM node_state_transitions WHERE from_state='RECONNECTING' AND to_state='CONNECTED'") != "0"
            raw_logs = compose("node", "logs", "--no-color", "--no-log-prefix", "arknet-agent", capture=True)
            events = []
            for line in raw_logs.splitlines():
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if event.get("msg") == "node_state_transition":
                    events.append((event.get("from_state"), event.get("to_state")))
            for transition in [("MESH", "DEGRADED"), ("DEGRADED", "RECONNECTING"),
                               ("RECONNECTING", "OFFLINE"), ("OFFLINE", "RECONNECTING"),
                               ("RECONNECTING", "CONNECTED")]:
                assert transition in events, (transition, events)
            assert token not in raw_logs and envs["node"]["POSTGRES_PASSWORD"] not in raw_logs
            assert not any(secret in raw_logs for secret in operator.secrets)
            print("PASS: UI proxy and structured logs expose the full MESH outage/reconnect transition path", flush=True)
            print("PASS: restored Core automatically reconnects using original credential, without re-enrollment", flush=True)

            compose("node", "restart", "arknet-agent")
            compose("node", "up", "-d", "--wait", "--wait-timeout", "120", "postgres", "arknet-agent")
            wait("heartbeat after Agent restart", lambda: (lambda d: d if d["last_seen_at"] != recovered["last_seen_at"] else None)(http("core", detail_path, admin=True)))
            assert sql("node", "SELECT row_to_json(e) FROM (SELECT status,core_url,enrolled_at FROM core_enrollment) e") == enrolled
            assert status()["last_successful_core_contact_at"] is not None
            print("PASS: saved enrollment resumes heartbeat after Agent restart", flush=True)

            for kind, component in [("core", "core"), ("node", "node-agent")]:
                subprocess.run(["docker", "build", "--target", "build", "-t", images[kind], str(ROOT / component)], check=True)
                env = dict(envs[kind], ARKNET_TEST_DATABASE_URL="postgresql://arknet:" + envs[kind]["POSTGRES_PASSWORD"] + "@postgres:5432/" + envs[kind]["POSTGRES_DB"] + "?sslmode=disable")
                subprocess.run(["docker", "run", "--rm", "--network", projects[kind] + "_" + kind,
                                "-e", "ARKNET_TEST_DATABASE_URL", "-e", "CGO_ENABLED=0", "-e", "GOMAXPROCS=2",
                                # Tests read these repository files relative to the module (WORKDIR /src).
                                "-v", str(ROOT / "VERSION") + ":/VERSION:ro",
                                "-v", str(ROOT / "docs") + ":/docs:ro",
                                "-v", str(ROOT / "deploy" / "node") + ":/deploy/node:ro",
                                "-v", str(ROOT / "node-ui" / "package.json") + ":/node-ui/package.json:ro", images[kind],
                                "go", "test", "-trimpath", "-p=1", "-count=1", "./..."], env=env, check=True)
            print("PASS: real PostgreSQL authenticated heartbeat, replay/race, migration and lifecycle tests", flush=True)
        except Exception:
            for kind in ports:
                compose(kind, "logs", "--tail", "15", check=False)
            raise
        finally:
            failures = []
            for kind in ["node", "core"]:
                try:
                    compose(kind, "down", "--volumes", "--remove-orphans")
                except Exception as e:
                    failures.append(e)
            for image in images.values():
                if subprocess.run(["docker", "image", "inspect", image], capture_output=True).returncode == 0:
                    subprocess.run(["docker", "image", "rm", image], check=True, capture_output=True)
            if created:
                subprocess.run(["docker", "network", "rm", network], check=True, capture_output=True)
            if failures:
                raise RuntimeError("Disposable heartbeat cleanup failed") from failures[0]
            print("Removed only disposable heartbeat resources: " + prefix, flush=True)


if __name__ == "__main__":
    main()
