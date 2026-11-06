#!/usr/bin/env python3
"""Verify media selection using disposable Core/Node stacks and real PostgreSQL.

No developer .env or existing stack is modified. Only uniquely named test
containers, networks, and volumes are removed. Requires Docker Compose/Python 3.
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


def main(with_transfer=False):
    prefix = "arknet-media-test-" + uuid.uuid4().hex[:12]
    network = prefix + "-transport"
    core_admin = None  # scoped token minted after Core starts
    transfer_token = secrets.token_hex(32)
    core_port, node_port = free_port(), free_port()
    allocation = 1 << 30  # One GiB, configured in GiB and accounted in bytes.
    envs = {}
    for kind in ["core", "node"]:
        envs[kind] = dict(os.environ, POSTGRES_USER="arknet", POSTGRES_DB="arknet_" + kind,
                          POSTGRES_PASSWORD=secrets.token_hex(32), PGSSLMODE="disable",
                          ARKNET_CORE_PORT=core_port, ARKNET_AGENT_PORT=node_port,
                          ARKNET_CORE_URL="http://arknet-core:8080", ARKNET_CORE_ALLOW_HTTP="true",
                          ARKNET_HEARTBEAT_INTERVAL="5s", ARKNET_MEDIA_STORAGE_ALLOCATION_GIB="1",
                          ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES="")
    if with_transfer:
        envs["node"].update(ARKNET_TRANSFER_TOKEN=transfer_token, ARKNET_TRANSFER_MOCK_DURATION="1h")
    core_url, node_url = "http://127.0.0.1:" + core_port, "http://127.0.0.1:" + node_port

    def request(base, path, method="GET", body=None, headers=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(base + path, data=data, method=method,
                                     headers={"Content-Type": "application/json", **(headers or {})})
        try:
            response = urllib.request.urlopen(req, timeout=20)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, json.load(response)

    def wait_state(expected):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            status, data = request(node_url, "/api/v1/status")
            if status == 200 and data["state"] == expected:
                return data
            time.sleep(0.25)
        raise AssertionError("Node did not reach " + expected)

    with tempfile.TemporaryDirectory(prefix=prefix) as directory:
        files = {}
        for kind, service, local in [("core", "arknet-core", "core"), ("node", "arknet-agent", "node")]:
            override = {"services": {service: {"networks": [local, "media-test"]}},
                        "networks": {"media-test": {"external": True, "name": network}}}
            if kind == "node":
                # This check does not need Jellyfin, a UI, or media volumes.
                override["services"][service]["environment"] = {"JELLYFIN_API_URL": ""}
            path = Path(directory) / (kind + ".json")
            path.write_text(json.dumps(override))
            files[kind] = path

        def compose(kind, *args, capture=False, check=True):
            base = ["docker", "compose", "-p", prefix + "-" + kind,
                    "-f", str(ROOT / "deploy" / kind / "compose.yaml")]
            if with_transfer and kind == "node":
                base += ["-f", str(ROOT / "deploy/node/compose.transfer-mock.yaml")]
            result = subprocess.run([*base, "-f", str(files[kind]), *args],
                                    cwd=ROOT / "deploy" / kind, env=envs[kind],
                                    check=check, text=True, capture_output=capture)
            return result.stdout.strip() if capture else None

        core_admin = ScopedAdmin(lambda *a, **k: compose("core", *a, **k), "media-selection-stack", "nodes.decide")

        def sql(kind, statement):
            return compose(kind, "exec", "-T", "postgres", "psql", "-U", "arknet", "-d", "arknet_" + kind,
                           "-At", "-v", "ON_ERROR_STOP=1", "-c", statement, capture=True)

        made_network = False
        try:
            subprocess.run(["docker", "network", "create", network], check=True)
            made_network = True
            compose("core", "up", "--build", "-d", "--wait", "--wait-timeout", "120")
            compose("node", "up", "--build", "-d", "--wait", "--wait-timeout", "120", "postgres", "arknet-agent")
            node = wait_state("ENROLLMENT_PENDING")
            status, _ = request(core_url, "/api/admin/v1/nodes/" + node["node_id"] + "/approve", "POST",
                                headers=core_admin.headers())
            assert status == 200
            wait_state("CONNECTED")

            first, second, removed = ["media_" + secrets.token_hex(32) for _ in range(3)]
            sql("core", "INSERT INTO media_objects (public_id,content_type,title,size,state) VALUES "
                f"('{first}','VIDEO','Selected movie',{allocation},'ACTIVE'),"
                f"('{second}','AUDIO','No room album',1,'ACTIVE'),"
                f"('{removed}','VIDEO','Removed movie',1,'REMOVED')")
            status, listing = request(node_url, "/api/v1/media")
            assert status == 200 and listing["core_available"] and listing["allocation_bytes"] == allocation
            assert len(listing["objects"]) == 3 and listing["reserved_bytes"] == 0
            assert all(o["local_state"] == "NOT_PRESENT" for o in listing["objects"])
            print("PASS: Node fetches the real signed Core catalog with local NOT_PRESENT states", flush=True)

            selection_path = "/api/v1/media/" + first + "/selection"
            status, selected = request(node_url, selection_path, "POST", {})
            assert status == 200 and selected["local_state"] == "QUEUED" and selected["size"] == allocation
            assert request(node_url, selection_path, "POST", {}) == (200, selected)
            assert request(node_url, "/api/v1/media/" + second + "/selection", "POST", {}) == (409, {"error": "storage_allocation_exceeded"})
            assert request(node_url, "/api/v1/media/" + removed + "/selection", "POST", {}) == (409, {"error": "media_removed"})
            assert sql("node", "SELECT count(*) FROM media_selections") == "1"
            assert request(node_url, "/api/v1/media")[1]["reserved_bytes"] == allocation
            print("PASS: operator queue is idempotent; allocation and removal checks reject new selections", flush=True)

            # Real process restart and recreation reuse the same local database.
            compose("node", "restart", "arknet-agent")
            compose("node", "up", "-d", "--wait", "--wait-timeout", "120", "arknet-agent")
            compose("node", "up", "-d", "--no-deps", "--force-recreate", "--wait", "--wait-timeout", "120", "arknet-agent")
            saved = next(o for o in request(node_url, "/api/v1/media")[1]["objects"] if o["id"] == first)
            assert saved == selected
            assert sql("node", "SELECT local_state FROM media_selections") == "QUEUED"
            print("PASS: queue survives Agent restart/recreation and does not start automatic downloads", flush=True)

            envs["node"]["ARKNET_MEDIA_STORAGE_ALLOCATION_GIB"] = "-1"
            compose("node", "up", "-d", "--no-deps", "--force-recreate", "--wait", "--wait-timeout", "120", "arknet-agent")
            assert request(node_url, "/api/v1/media")[1]["allocation_bytes"] == -1
            status, extra = request(node_url, "/api/v1/media/" + second + "/selection", "POST", {})
            assert status == 200 and extra["local_state"] == "QUEUED"
            assert request(node_url, "/api/v1/media")[1]["reserved_bytes"] == allocation + 1
            assert request(node_url, "/api/v1/media/" + removed + "/selection", "POST", {}) == (409, {"error": "media_removed"})
            print("PASS: -1 GiB configuration removes the allocation limit without bypassing removal checks", flush=True)

            additional = None
            if with_transfer:
                additional = check_mock_transfers(request, node_url, sql, compose, first, second, allocation)

            reserved = allocation + 1 + (additional["size"] if additional is not None else 0)
            sql("core", f"UPDATE media_objects SET state='REMOVED',updated_at=clock_timestamp() WHERE public_id='{first}'")
            listing = request(node_url, "/api/v1/media")[1]
            retained = next(o for o in listing["objects"] if o["id"] == first)
            assert retained["local_state"] == "REMOVED_RETAINED" and retained["state"] == "REMOVED"
            assert listing["reserved_bytes"] == reserved
            compose("core", "stop", "arknet-core")
            compose("node", "restart", "arknet-agent")
            compose("node", "up", "-d", "--wait", "--wait-timeout", "120", "arknet-agent")
            status, offline = request(node_url, "/api/v1/media")
            assert status == 200 and not offline["core_available"] and offline["allocation_bytes"] == -1
            expected = {first: retained, second: extra}
            if additional is not None:
                expected[additional["id"]] = additional
            assert {o["id"]: o for o in offline["objects"]} == expected
            assert offline["reserved_bytes"] == reserved
            assert request(node_url, "/api/v1/media/" + second + "/selection", "POST", {}) == (503, {"error": "catalog_unavailable"})
            print("PASS: removed selection and reservation persist offline; new selections fail closed", flush=True)
        finally:
            try:
                compose("node", "down", "--volumes", "--remove-orphans")
            finally:
                try:
                    compose("core", "down", "--volumes", "--remove-orphans")
                finally:
                    if made_network:
                        subprocess.run(["docker", "network", "rm", network], check=True)
            print("Removed only disposable media test stacks: " + prefix, flush=True)


def check_mock_transfers(request, node_url, sql, compose, first, second, allocation):
    def action(object_id, operation, expected=200):
        status, result = request(node_url, f"/api/v1/media/{object_id}/transfer/{operation}", "POST", {})
        assert status == expected, (operation, status, result)
        return result

    def statuses():
        status, result = request(node_url, "/api/v1/transfers")
        assert status == 200 and result["enabled"] and result["available"]
        return {s["object_id"]: s for s in result["transfers"]}

    def report(object_id, operation, body=None):
        # Worker simulation inside the isolated sidecar. Its local token never
        # appears in host arguments, output, Agent responses, or Core requests.
        command = ('wget -qO- --header="Authorization: Bearer $ARKNET_TRANSFER_TOKEN" '
                   '--header="Content-Type: application/json" --post-data="$1" "$2"')
        data = compose("node", "exec", "-T", "arknet-transfer", "sh", "-c", command, "report",
                       json.dumps(body or {}), f"http://127.0.0.1:8084/api/transfer/v1/mock/transfers/{object_id}/{operation}", capture=True)
        return json.loads(data)

    config = json.loads(compose("node", "config", "--format", "json", capture=True))
    sidecar = config["services"]["arknet-transfer"]
    assert not sidecar.get("ports") and not sidecar.get("volumes")
    assert set(sidecar["networks"]) == {"transfer-control"}
    assert config["networks"]["transfer-control"]["internal"]
    assert not any(k.startswith("PG") or "CORE" in k or k == "DATABASE_URL" for k in sidecar["environment"])
    assert statuses() == {}, "GET and queuing must not automatically start transfers"
    first_status = action(first, "start")
    assert first_status["mock"] and first_status["state"] == "DOWNLOADING"
    assert first_status["total_bytes"] == allocation
    report(first, "progress", {"completed_bytes": allocation // 4})
    progress = statuses()[first]
    assert 0 < progress["completed_bytes"] < allocation
    paused = action(first, "pause")
    time.sleep(0.05)
    assert statuses()[first]["completed_bytes"] == paused["completed_bytes"]
    assert action(first, "resume")["state"] == "DOWNLOADING"
    report(first, "complete")
    assert statuses()[first]["state"] == "COMPLETED"
    assert sql("node", f"SELECT local_state FROM media_selections WHERE id='{first}'") == "QUEUED"
    print("PASS: Agent drives authorized fake progress/pause/resume/completion without claiming real availability", flush=True)

    action(second, "start")
    report(second, "verification-failure")
    failed = statuses()[second]
    assert failed["state"] == "VERIFICATION_FAILED" and failed["error_code"] == "verification_failed"
    third = "media_" + secrets.token_hex(32)
    sql("core", f"INSERT INTO media_objects (public_id,content_type,title,size,state) VALUES ('{third}','VIDEO','Cancel test',128,'ACTIVE')")
    status, selected = request(node_url, f"/api/v1/media/{third}/selection", "POST", {})
    assert status == 200
    action(third, "start")
    assert action(third, "cancel")["state"] == "CANCELLED"
    action(third, "resume", 409)
    listing = statuses()
    assert set(listing) == {first, second, third}
    assert all(s["mock"] for s in listing.values())
    print("PASS: verification-failure reporting and terminal cancellation; isolated sidecar has no Core/DB credentials", flush=True)
    return selected


if __name__ == "__main__":
    main()
