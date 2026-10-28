#!/usr/bin/env python3
"""Prompt 43 ArkNet MVP end-to-end acceptance run: Core, Node A and Node B.

Topology (every resource uses one random prefix and is removed afterwards):

* ``wan``     - simulated Internet: Core (``arknet-core``), private tracker and
                both Nodes' transfer services (the only peer-to-peer path).
* ``uplink-a`` / ``uplink-b`` - each Node Agent's private outbound path to Core.
* each Node keeps its own Compose-private PostgreSQL, Jellyfin and control
  networks; nothing else is shared between Node A and Node B.

The run exercises the real product paths only (no test seeding adapters) and
records PASS/FAIL per Prompt 43 criterion. Items 1, 17 and 18 are covered by
``check-node-install-vm.py`` and ``check-node-upgrade-stack.py``; the report
generator merges their results. Known secrets never reach stdout.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import runpy
import secrets
import shutil
import subprocess
import tempfile
import time
import traceback
import urllib.parse
import uuid

ROOT = Path(__file__).resolve().parent.parent
PROMPT25 = Path(__file__).with_name("check-core-to-node-transfer-stack.py")

ITEMS = {
    2: "Enrollment", 3: "Node restart", 4: "Global user", 5: "Local-only user",
    6: "Pairwise node membership", 7: "Jellyfin", 8: "Media selection",
    9: "BitTorrent v2 Core -> Node transfer", 10: "Interrupted transfer/resume",
    11: "Node A -> Node B swarm contribution", 12: "Offline Jellyfin playback",
    13: "Cached global authentication", 14: "Core reconnect",
    15: "Removed-media retention", 16: "Node revocation",
    19: "Operator cannot enumerate global users",
    20: "Node A cannot arbitrarily access Node B",
}


class SetupOnly(Exception):
    """Raised after setup when --setup-only was requested."""


def verify_bind_sources(run, image, paths):
    """Fail fast when Docker cannot see a host bind source.

    Docker Desktop creates an empty directory in place of a file it cannot see
    (for example under an unshared host path), which later surfaces as a
    misleading "is a directory" error inside Core or the tracker.
    """
    for kind, path in paths:
        flag = "-f" if kind == "file" else "-d"
        result = subprocess.run(["docker", "run", "--rm", "--network", "none", "--entrypoint", "sh",
                                 "-v", f"{path}:/probe:ro", image, "-c", f"test {flag} /probe"],
                                capture_output=True, text=True, timeout=120, check=False)
        if result.returncode != 0:
            raise RuntimeError(f"Docker cannot see bind source {Path(path).name!r} as a {kind}; run the harness from a"
                               " Docker-shared path such as the repository checkout under the user's home directory")


class Results:
    def __init__(self):
        self.items = {n: {"item": n, "name": name, "status": "NOT_RUN", "evidence": []} for n, name in ITEMS.items()}

    def evidence(self, item, text):
        self.items[item]["evidence"].append(text)
        print(f"  [{item}] {text}", flush=True)

    def passed(self, item):
        if self.items[item]["status"] != "FAIL":
            self.items[item]["status"] = "PASS"
        print(f"PASS {item}: {ITEMS[item]}", flush=True)

    def failed(self, item, message):
        self.items[item]["status"] = "FAIL"
        self.items[item]["error"] = message
        print(f"FAIL {item}: {ITEMS[item]}: {message}", flush=True)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--results", help="write sanitized per-item JSON results")
    parser.add_argument("--setup-only", action="store_true",
                        help="start Core, Node A and Node B, print SETUP:, then clean up without checking items")
    args = parser.parse_args(argv)

    p25 = runpy.run_path(str(PROMPT25), run_name="arknet_prompt25_reuse")
    p25["verify_prerequisites"]()
    run, request, request_json = p25["run"], p25["request"], p25["request_json"]
    wait_until, jellyfin_setup, free_port = p25["wait_until"], p25["jellyfin_setup"], p25["free_port"]

    results = Results()
    prefix = "arknet-p43-test-" + uuid.uuid4().hex[:12]
    wan, uplink_a, uplink_b = prefix + "-wan", prefix + "-uplink-a", prefix + "-uplink-b"
    networks = (wan, uplink_a, uplink_b)
    core_project = prefix + "-core"
    images = {"core": prefix + "-core-image", "tracker": prefix + "-tracker-image",
              "agent": prefix + "-agent-image", "transfer": prefix + "-transfer-image",
              "helper": prefix + "-libtorrent", "agent-build": prefix + "-agent-build"}
    temporary = Path(tempfile.mkdtemp(prefix="." + prefix + "-", dir=ROOT)); os.chmod(temporary, 0o755)
    made_networks, known_secrets = set(), []
    failure = None

    admin_token, core_password = secrets.token_hex(32), secrets.token_hex(32)
    global_password, local_password = "global-" + secrets.token_urlsafe(18), "local-" + secrets.token_urlsafe(18)
    known_secrets.extend((admin_token, core_password, global_password, local_password))
    core_port = free_port()
    core_base = f"http://127.0.0.1:{core_port}"
    core_env = dict(os.environ, POSTGRES_USER="arknet", POSTGRES_DB="arknet_core", POSTGRES_PASSWORD=core_password,
                    PGSSLMODE="disable", ARKNET_CORE_PORT=str(core_port), ARKNET_CORE_DEV_ADMIN_ENABLED="true",
                    ARKNET_CORE_DEV_ADMIN_TOKEN=admin_token, ARKNET_TRACKER_PORT=str(free_port()),
                    ARKNET_TRACKER_ANNOUNCE_URL="http://arknet-tracker:6969", ARKNET_TRACKER_ANNOUNCE_INTERVAL="5s",
                    ARKNET_TRACKER_PEER_TTL="30s", ARKNET_CORE_WEB_SEED_ORIGIN="http://arknet-core:8080")
    core_override = temporary / "core-override.json"
    source = temporary / "source"; source.mkdir(mode=0o755)

    nodes = {}
    for label, uplink in (("a", uplink_a), ("b", uplink_b)):
        password, token, jelly_password = secrets.token_hex(32), secrets.token_hex(32), secrets.token_urlsafe(24)
        known_secrets.extend((password, token, jelly_password))
        cfg = {"project": prefix + "-node-" + label, "uplink": uplink, "agent_port": free_port(),
               "jellyfin_port": free_port(), "password": password, "jelly_password": jelly_password,
               "override": temporary / f"node-{label}-override.json",
               "jelly_config": temporary / f"jellyfin-{label}-config", "jelly_cache": temporary / f"jellyfin-{label}-cache"}
        cfg["jelly_config"].mkdir(mode=0o755); cfg["jelly_cache"].mkdir(mode=0o755)
        cfg["env"] = dict(os.environ, POSTGRES_USER="arknet", POSTGRES_DB="arknet_node", POSTGRES_PASSWORD=password,
                          PGSSLMODE="disable", ARKNET_AGENT_PORT=str(cfg["agent_port"]),
                          JELLYFIN_BIND=f"127.0.0.1:{cfg['jellyfin_port']}", ARKNET_CORE_URL="http://arknet-core:8080",
                          ARKNET_CORE_ALLOW_HTTP="true", ARKNET_HEARTBEAT_INTERVAL="5s",
                          ARKNET_MEDIA_STORAGE_ALLOCATION_GIB="1", ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES="",
                          ARKNET_TRANSFER_TOKEN=token, ARKNET_TRANSFER_TRACKER_ORIGIN="http://arknet-tracker:6969",
                          ARKNET_TRANSFER_TRACKER_ALLOW_HTTP="true", ARKNET_TRANSFER_WEB_SEED_ORIGIN="http://arknet-core:8080",
                          ARKNET_TRANSFER_WEB_SEED_ALLOW_HTTP="true", ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT=str(512 * 1024),
                          # Refreshing grants replaces the worker's web-seed URL.
                          # Leave a bounded transfer window between refreshes;
                          # a 5s stress cadence continually tears down HTTP peers.
                          ARKNET_TRANSFER_RECONCILE_INTERVAL="30s", JELLYFIN_API_KEY="")
        cfg["base"] = f"http://127.0.0.1:{cfg['agent_port']}"
        cfg["jelly_base"] = f"http://127.0.0.1:{cfg['jellyfin_port']}"
        nodes[label] = cfg
    projects = (core_project, nodes["a"]["project"], nodes["b"]["project"])
    admin = {"Authorization": "Bearer " + admin_token}

    def core_compose(*items, capture=False, check=True, timeout=600):
        return run(["docker", "compose", "-p", core_project, "-f", str(ROOT / "deploy/core/compose.yaml"),
                    "-f", str(ROOT / "deploy/core/compose.tracker.yaml"), "-f", str(ROOT / "deploy/core/compose.web-seed.yaml"),
                    "-f", str(core_override), *items], cwd=ROOT / "deploy/core", env=core_env,
                   capture=capture, check=check, timeout=timeout)

    def node_compose(label, *items, capture=False, check=True, timeout=600):
        cfg = nodes[label]
        return run(["docker", "compose", "-p", cfg["project"], "-f", str(ROOT / "deploy/node/compose.yaml"),
                    "-f", str(p25["REAL_TRANSFER"]), "-f", str(cfg["override"]), *items], cwd=ROOT / "deploy/node",
                   env=cfg["env"], capture=capture, check=check, timeout=timeout)

    def psql(compose, database, statement):
        return compose("exec", "-T", "postgres", "psql", "-U", "arknet", "-d", database, "-At", "-v", "ON_ERROR_STOP=1",
                       "-c", statement, capture=True)

    def core_sql(statement):
        return psql(core_compose, "arknet_core", statement)

    def node_sql(label, statement):
        return psql(lambda *i, **k: node_compose(label, *i, **k), "arknet_node", statement)

    def dump(compose, database):
        return compose("exec", "-T", "postgres", "pg_dump", "-U", "arknet", "-d", database, capture=True, timeout=120)

    def agent(label, path, method="GET", body=None, expected=(200,), headers=None):
        return request_json(nodes[label]["base"] + path, method, body, headers, expected=expected)[2]

    def status(label):
        return agent(label, "/api/v1/status")

    def in_state(label, *states):
        value = status(label)
        return value if value["state"] in states else None

    def media_object(label, object_id):
        return next((o for o in agent(label, "/api/v1/media")["objects"] if o["id"] == object_id), None)

    def transfer_status(label, object_id):
        value = agent(label, "/api/v1/transfers")
        return next((t for t in value["transfers"] if t["object_id"] == object_id), None)

    def container(compose, service):
        return compose("ps", "-q", service, capture=True)

    def transfer_seeds(label):
        script = ("import os,urllib.request; r=urllib.request.Request("
                  "'http://127.0.0.1:8084/api/transfer/v1/seeds',headers={"
                  "'Authorization':'Bearer '+os.environ['ARKNET_TRANSFER_TOKEN']});"
                  "print(urllib.request.urlopen(r,timeout=3).read().decode())")
        cid = container(lambda *i, **k: node_compose(label, *i, **k), "arknet-transfer")
        return json.loads(run(["docker", "exec", cid, "python3", "-c", script], capture=True))["seeds"]

    def jellyfin_stream(label, digest, size):
        cfg = nodes[label]
        auth = {"X-Emby-Token": cfg["jelly_access"]}
        request_json(cfg["jelly_base"] + "/Library/Refresh", "POST", {}, auth, expected=(204,))
        item = wait_until("Jellyfin indexed " + label, lambda: next((e for e in request_json(
            cfg["jelly_base"] + "/Items?Recursive=true&IncludeItemTypes=Movie&Fields=Path", headers=auth)[2].get("Items", [])
            if e.get("Type") == "Movie"), None), 180)
        stream_status, _, streamed = request(cfg["jelly_base"] + "/Videos/" + item["Id"] + "/stream.mp4?" +
                                             urllib.parse.urlencode({"Static": "true", "api_key": cfg["jelly_key"]}), timeout=180)
        return stream_status == 200 and len(streamed) == size and hashlib.sha256(streamed).hexdigest() == digest

    def item(n, fn):
        try:
            fn()
            results.passed(n)
            return True
        except Exception as error:  # record and continue with independent criteria
            message = f"{type(error).__name__}: {error}"
            for value in sorted((v for v in known_secrets if v), key=len, reverse=True):
                message = message.replace(value, "[REDACTED]")
            results.failed(n, message[:800])
            traceback.print_exc()
            return False

    def make_video(name, seconds, frequency):
        path = source / name
        run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30",
             "-f", "lavfi", "-i", f"sine=frequency={frequency}:sample_rate=48000", "-t", str(seconds), "-c:v", "libx264",
             "-preset", "ultrafast", "-b:v", "5M", "-maxrate", "5M", "-bufsize", "10M", "-pix_fmt", "yuv420p",
             "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", "-f", "mp4", str(path)], timeout=300)
        make_torrent = temporary / "make-torrent.py"
        torrent = json.loads(run(["docker", "run", "--rm", "-v", f"{temporary}:/work", "-v", f"{make_torrent}:/make-torrent.py:ro",
                                  images["helper"], "/make-torrent.py", "/work", name], capture=True, timeout=180))
        if torrent["size"] != path.stat().st_size or torrent["pieces"] <= 1:
            raise AssertionError("invalid private v2 test torrent")
        shutil.copyfile(temporary / "object.torrent", source / (name + ".torrent"))
        core_sql("INSERT INTO media_objects (public_id,content_type,title,size,state,info_hash_v2) VALUES "
                 f"('{name}','VIDEO','Prompt 43 acceptance',{torrent['size']},'ACTIVE','{torrent['info_hash_v2']}')")
        return {"id": name, "size": torrent["size"], "hash": torrent["info_hash_v2"],
                "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}

    try:
        # ---------------------------------------------------------------- setup
        key_helper = temporary / "grant-helper.go"; key_helper.write_text(p25["KEY_HELPER"]); os.chmod(key_helper, 0o600)
        (temporary / "make-torrent.py").write_text(p25["MAKE_TORRENT"])
        run(["go", "run", str(key_helper), "keygen", str(temporary)], timeout=120)
        private_key, public_key = temporary / "tracker-private.key", temporary / "tracker-public.key"
        os.chmod(private_key, 0o644)  # disposable key; Core's non-root UID reads the bind mount
        core_env.update(ARKNET_TRACKER_SIGNING_KEY_FILE=str(private_key), ARKNET_TRACKER_VERIFY_KEY_FILE=str(public_key),
                        ARKNET_CORE_WEB_SEED_SOURCE=str(source))
        verify_bind_sources(run, "postgres:18-alpine", (("file", private_key), ("file", public_key), ("directory", source)))
        print("PREFLIGHT: Docker sees the tracker key files and web-seed directory", flush=True)
        run(["docker", "build", "-t", images["helper"], "-"], input_text=p25["LIBTORRENT_DOCKERFILE"], timeout=900)
        run(["docker", "build", "--target", "build", "-t", images["agent-build"], str(ROOT / "node-agent")], timeout=900)
        for network in networks:
            run(["docker", "network", "create", network], capture=True); made_networks.add(network)
        core_override.write_text(json.dumps({"services": {
            "arknet-core": {"image": images["core"], "networks": ["core", "wan"]},
            "arknet-tracker": {"image": images["tracker"], "networks": ["tracker-edge", "wan"]}},
            "networks": {"wan": {"external": True, "name": wan}}}))
        for label, cfg in nodes.items():
            cfg["override"].write_text(json.dumps({"services": {
                "arknet-agent": {"image": images["agent"], "networks": ["node", "media-services", "transfer-control", "uplink"]},
                "arknet-transfer": {"image": images["transfer"], "networks": ["transfer-control", "wan"]},
                "jellyfin": {"image": "jellyfin/jellyfin:10.10.7", "volumes": [
                    f"{cfg['jelly_config']}:/config", f"{cfg['jelly_cache']}:/cache", "media:/srv/arknet/media:ro"]}},
                "networks": {"uplink": {"external": True, "name": cfg["uplink"]}, "wan": {"external": True, "name": wan}}}))
            os.chmod(cfg["override"], 0o600)
        os.chmod(core_override, 0o600)

        core_compose("up", "--build", "-d", "--wait", "--wait-timeout", "240")
        core_id = container(core_compose, "arknet-core")
        for label, cfg in nodes.items():
            run(["docker", "network", "connect", "--alias", "arknet-core", cfg["uplink"], core_id])
        for label, cfg in nodes.items():
            build = ["--build"] if label == "a" else []
            node_compose(label, "up", *build, "-d", "--wait", "--wait-timeout", "300", "postgres", "jellyfin", "arknet-transfer")
            wait_until(label + " Jellyfin HTTP", lambda c=cfg: request(c["jelly_base"] + "/System/Info/Public")[0] == 200, 180)
            access, key = jellyfin_setup(cfg["jelly_base"], cfg["jelly_password"])
            known_secrets.extend((access, key)); cfg["jelly_access"], cfg["jelly_key"] = access, key
            cfg["env"]["JELLYFIN_API_KEY"] = key
            node_compose(label, "up", "-d", "--wait", "--wait-timeout", "180", "arknet-agent")
        print("SETUP: Core, tracker, web seed, Node A and Node B are running", flush=True)
        if args.setup_only:
            raise SetupOnly()

        # ----------------------------------------------------------- 2 enroll
        def enrollment():
            for label, cfg in nodes.items():
                pending = wait_until(label + " pending", lambda l=label: in_state(l, "ENROLLMENT_PENDING"), 120)
                cfg["node_id"] = pending["node_id"]
            assert nodes["a"]["node_id"] != nodes["b"]["node_id"]
            listed = request_json(core_base + "/api/admin/v1/nodes/pending", headers=admin)[2]["nodes"]
            assert {n["node_id"] for n in listed} == {nodes["a"]["node_id"], nodes["b"]["node_id"]}
            results.evidence(2, "both fresh Nodes self-enrolled outbound and are listed PENDING in Core")
            for label, cfg in nodes.items():
                decision = request_json(core_base + f"/api/admin/v1/nodes/{cfg['node_id']}/approve", "POST", headers=admin)[2]
                assert decision["enrollment_status"] == "APPROVED" and decision["trust_status"] == "TRUSTED"
                wait_until(label + " connected", lambda l=label: in_state(l, "CONNECTED"), 120)
            keys = core_sql("SELECT count(DISTINCT enrollment_public_key) FROM nodes")
            assert keys == "2", keys
            results.evidence(2, "approval completed the signed handshake; both CONNECTED with 2 distinct pinned keys")
        if not item(2, enrollment):
            raise RuntimeError("enrollment failed; dependent criteria cannot run")

        # ------------------------------------------------------- 4 global user
        def global_user():
            created = request_json(core_base + "/api/admin/v1/users", "POST",
                                   {"username": "alice-e2e", "password": global_password}, admin, expected=(201,))[2]
            nodes["user_id"] = created["user_id"]
            login = request_json(core_base + "/api/identity/v1/sessions", "POST",
                                 {"username": "alice-e2e", "password": global_password}, expected=(200, 201))[2]
            nodes["core_session"] = login["token"]; known_secrets.append(login["token"])
            session = request_json(core_base + "/api/identity/v1/session", headers={"Authorization": "Bearer " + login["token"]})[2]
            assert session["user_id"] == created["user_id"]
            wrong = request(core_base + "/api/identity/v1/sessions", "POST", {"username": "alice-e2e", "password": global_password + "x"})
            assert wrong[0] == 401
            results.evidence(4, "Core created a global user, issued a session, and rejected a wrong password")
        item(4, global_user)

        # ---------------------------------------------------- 5 local-only user
        def local_user():
            created = agent("a", "/api/v1/local-users", "POST", {"username": "local-bob", "password": local_password}, (201,))
            assert created["kind"] == "LOCAL"
            nodes["local_id"] = created["user_id"]
            agent("a", "/api/v1/local-users/sessions", "POST", {"username": "local-bob", "password": local_password}, (201,))
            other = request(nodes["b"]["base"] + "/api/v1/local-users/sessions", "POST",
                            {"username": "local-bob", "password": local_password})
            assert other[0] == 401
            core_dump = dump(core_compose, "arknet_core")
            assert "local-bob" not in core_dump and created["user_id"] not in core_dump and created["user_id"][6:] not in core_dump
            results.evidence(5, "local user authenticates on Node A only; Core database dump has no trace of it")
        item(5, local_user)

        # ------------------------------------------------- 6 pairwise membership
        def pairwise():
            expires = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 3600))
            member_ids = {}
            for label in ("a", "b"):
                invitation = agent(label, "/api/v1/invitations", "POST", {"role": "MEMBER", "expires_at": expires, "max_uses": 1}, (201,))
                known_secrets.append(invitation["secret"])
                accepted = request_json(core_base + "/api/identity/v1/invitations/accept", "POST", {"secret": invitation["secret"]},
                                        {"Authorization": "Bearer " + nodes["core_session"]}, expected=(201,))[2]
                assert accepted["node_id"] == nodes[label]["node_id"]
                member_ids[label] = accepted["member_id"]
            assert member_ids["a"] != member_ids["b"]
            nodes["a"]["member_id"], nodes["b"]["member_id"] = member_ids["a"], member_ids["b"]
            for label, other in (("a", "b"), ("b", "a")):
                text = dump(lambda *i, **k: node_compose(label, *i, **k), "arknet_node")
                for value in ("alice-e2e", nodes["user_id"], nodes["user_id"][5:], member_ids[other], nodes[other]["node_id"]):
                    assert value not in text, f"Node {label.upper()} database contains a correlating identifier"
            results.evidence(6, "one global user received different random member IDs on A and B; neither Node stores the"
                                " username, global user ID, the other Node's member ID, or the other Node's ID")
        item(6, pairwise)

        # ------------------------------------------------------------ 7 Jellyfin
        def jellyfin():
            for label in ("a", "b"):
                assert status(label)["jellyfin_healthy"] is True
            results.evidence(7, "both Agents report local Jellyfin healthy; Jellyfin set up with ArkNet library on read-only media")
        item(7, jellyfin)

        # ------------------------------------------- 3 restart + 8 selection etc.
        video = make_video("media_" + secrets.token_hex(32), 35, 880)
        later = make_video("media_" + secrets.token_hex(32), 12, 440)

        def selection():
            listed = media_object("a", video["id"])
            assert listed and listed["local_state"] == "NOT_PRESENT"
            assert agent("a", f"/api/v1/media/{video['id']}/selection", "POST", {})["local_state"] == "QUEUED"
            results.evidence(8, "signed catalog fetch listed the object; operator selection queued it")
        item(8, selection)

        def restart():
            before = status("a")
            node_compose("a", "restart", "arknet-agent", timeout=120)
            wait_until("A after restart", lambda: in_state("a", "CONNECTED"), 120)
            node_compose("a", "up", "-d", "--force-recreate", "--wait", "--wait-timeout", "180", "arknet-agent")
            after = wait_until("A after recreate", lambda: in_state("a", "CONNECTED"), 120)
            assert after["node_id"] == before["node_id"]
            assert core_sql("SELECT count(*) FROM nodes") == "2"
            assert media_object("a", video["id"])["local_state"] == "QUEUED"
            assert request(nodes["a"]["base"] + "/api/v1/local-users/sessions", "POST",
                           {"username": "local-bob", "password": local_password})[0] == 201
            results.evidence(3, "Agent restart and container recreation kept node ID, CONNECTED enrollment, queued selection"
                                " and local users; Core still has exactly 2 Nodes")
        item(3, restart)

        # --------------------------------------------- 9 transfer + 10 resume
        def transfer():
            started = agent("a", f"/api/v1/media/{video['id']}/transfer/start", "POST", {})
            assert started["state"] == "DOWNLOADING" and started["mock"] is False
            partial = wait_until("partial", lambda: (lambda s: s if s and 2 << 20 <= s["completed_bytes"] < video["size"] else None)(
                transfer_status("a", video["id"])), 180, .25)
            results.evidence(9, f"real libtorrent transfer started from the Core web seed ({partial['completed_bytes']} bytes)")
            core_compose("stop", "arknet-core")
            before = transfer_status("a", video["id"])["completed_bytes"]
            node_compose("a", "restart", "arknet-transfer", timeout=120)
            after = wait_until("transfer API", lambda: transfer_status("a", video["id"]), 120)["completed_bytes"]
            assert after >= before - 256 * 1024, (before, after)
            results.evidence(10, f"Core stopped and transfer container restarted mid-download; progress retained ({after} bytes)")
            core_compose("start", "arknet-core")
            wait_until("Core", lambda: request(core_base + "/health")[0] == 200, 90)
            request_json(nodes["a"]["base"] + f"/api/v1/media/{video['id']}/transfer/resume", "POST", {}, expected=(200, 409))
            wait_until("AVAILABLE", lambda: (media_object("a", video["id"]) or {}).get("local_state") == "AVAILABLE", 300)
            final = transfer_status("a", video["id"])
            assert final["state"] == "COMPLETED" and final["completed_bytes"] == video["size"]
            media_hash = run(["docker", "run", "--rm", "-v", nodes["a"]["project"] + "_media:/m:ro", images["helper"], "-c",
                              f"import hashlib,pathlib;print(hashlib.sha256(pathlib.Path('/m/{video['id']}.mp4').read_bytes()).hexdigest())"],
                             capture=True)
            assert media_hash == video["sha256"]
            replica = core_sql(f"SELECT count(*) FROM media_replicas r JOIN nodes n ON n.id=r.node_id JOIN media_objects m ON "
                               f"m.id=r.media_object_id WHERE n.public_id='{nodes['a']['node_id']}' AND m.public_id='{video['id']}'")
            assert replica == "1"
            results.evidence(9, "object verified AVAILABLE with exact SHA-256 and a signed replica report in Core")
            results.evidence(10, "fresh authorization resumed retained pieces to completion")
        ok = item(9, transfer)
        if ok:
            results.passed(10)
        else:
            results.failed(10, "depends on item 9")

        # -------------------------------- 13a cached auth online population
        def online_global_login():
            session = agent("a", "/api/v1/global-users/sessions", "POST",
                            {"member_id": nodes["a"]["member_id"], "password": global_password}, (200, 201))
            assert session["kind"] == "GLOBAL" and session["member_id"] == nodes["a"]["member_id"]
            results.evidence(13, "online global-member login on Node A verified by Core and cached Node-locally")
        item(13, online_global_login)

        # ------------------------- 12 offline playback, 13b offline auth, 14
        def offline():
            core_compose("stop", "arknet-core", "arknet-tracker")
            wait_until("A offline", lambda: in_state("a", "DEGRADED", "OFFLINE"), 120)
            results.evidence(14, "Core outage moved Node A to " + status("a")["state"] + " while the Agent kept serving")
        item(14, offline)

        def offline_playback():
            assert jellyfin_stream("a", video["sha256"], video["size"])
            results.evidence(12, "with Core and tracker stopped, Jellyfin on Node A streamed the exact completed MP4")
        item(12, offline_playback)

        def offline_auth():
            session = agent("a", "/api/v1/global-users/sessions", "POST",
                            {"member_id": nodes["a"]["member_id"], "password": global_password}, (200, 201))
            assert session["kind"] == "GLOBAL"
            denied = request(nodes["a"]["base"] + "/api/v1/global-users/sessions", "POST",
                             {"member_id": nodes["a"]["member_id"], "password": global_password + "x"})
            assert denied[0] == 401
            foreign = request(nodes["a"]["base"] + "/api/v1/global-users/sessions", "POST",
                              {"member_id": nodes["b"]["member_id"], "password": global_password})
            assert foreign[0] == 401
            local = request(nodes["a"]["base"] + "/api/v1/local-users/sessions", "POST", {"username": "local-bob", "password": local_password})
            assert local[0] == 201
            results.evidence(13, "Core offline: cached member authenticated; wrong password and Node B's member ID rejected;"
                                 " local user still authenticates")
        item(13, offline_auth)

        def reconnect():
            core_compose("start", "arknet-core", "arknet-tracker")
            wait_until("Core", lambda: request(core_base + "/health")[0] == 200, 90)
            wait_until("A reconnected", lambda: in_state("a", "CONNECTED"), 180)
            assert status("a")["node_id"] == nodes["a"]["node_id"] and core_sql("SELECT count(*) FROM nodes") == "2"
            assert core_sql(f"SELECT enrollment_status||'/'||trust_status FROM nodes WHERE public_id='{nodes['a']['node_id']}'") == "APPROVED/TRUSTED"
            results.evidence(14, "Core restored; Node A reconnected automatically to CONNECTED with no re-enrollment")
        item(14, reconnect)

        # --------------------------------------------- 11 A -> B contribution
        def swarm():
            agent("a", "/api/v1/settings/peer-serving", "PUT", {"enabled": True})
            wait_until("A peer serving reported", lambda: core_sql(
                f"SELECT peer_serving FROM nodes WHERE public_id='{nodes['a']['node_id']}'") == "t", 60)
            eligible = (f"SELECT n.public_id FROM media_replicas r JOIN nodes n ON n.id=r.node_id JOIN media_objects m ON "
                        f"m.id=r.media_object_id WHERE m.public_id='{video['id']}' AND n.peer_serving")
            wait_until("A eligible source", lambda: core_sql(eligible) == nodes["a"]["node_id"], 60)
            wait_until("A product seed SEEDING", lambda: any(
                s["object_id"] == video["id"] and s["state"] == "SEEDING" for s in transfer_seeds("a")), 90)
            results.evidence(11, "node-agent seed reconciler -> transfer service -> libtorrent worker")
            agent("b", f"/api/v1/media/{video['id']}/selection", "POST", {})
            started = agent("b", f"/api/v1/media/{video['id']}/transfer/start", "POST", {})
            assert started["mock"] is False
            # Torrent metadata now comes from Core. Then remove Core's web seed from the
            # simulated Internet: the private tracker stays reachable, so the only
            # remaining payload source for Node B is Node A's real transfer service.
            wait_until("B metadata", lambda: (transfer_status("b", video["id"]) or {}).get("state") == "DOWNLOADING", 60)
            before_disconnect = transfer_status("b", video["id"])["completed_bytes"]
            assert 0 <= before_disconnect < video["size"]
            run(["docker", "network", "disconnect", wan, core_id])
            try:
                done = wait_until("B AVAILABLE from Node A", lambda: (lambda s: s if s and s["completed_bytes"] == video["size"] else None)(
                    transfer_status("b", video["id"])), 300)
            finally:
                run(["docker", "network", "connect", "--alias", "arknet-core", wan, core_id])
            assert done["state"] == "COMPLETED"
            wait_until("B AVAILABLE", lambda: (media_object("b", video["id"]) or {}).get("local_state") == "AVAILABLE", 180)
            b_hash = run(["docker", "run", "--rm", "-v", nodes["b"]["project"] + "_media:/m:ro", images["helper"], "-c",
                          f"import hashlib,pathlib;print(hashlib.sha256(pathlib.Path('/m/{video['id']}.mp4').read_bytes()).hexdigest())"],
                         capture=True)
            assert b_hash == video["sha256"]
            results.evidence(11, "with Core's web seed unreachable, Node B completed the exact object from Node A's product"
                                 " transfer service (private v2 swarm, tracker-gated)")
        item(11, swarm)

        # ----------------------------------------------- 15 removed media
        def removed():
            removed_object = request_json(core_base + f"/api/admin/v1/media/{video['id']}/remove", "POST", headers=admin)[2]
            assert removed_object["state"] == "REMOVED"
            retained = wait_until("A REMOVED_RETAINED", lambda: (lambda o: o if o and o["local_state"] == "REMOVED_RETAINED" else None)(
                media_object("a", video["id"])), 120)
            results.evidence(15, "Core tombstone propagated; Node A state " + retained["local_state"])
            assert jellyfin_stream("a", video["sha256"], video["size"])
            again = request(nodes["a"]["base"] + f"/api/v1/media/{video['id']}/selection", "POST", {})
            assert again[0] == 409
            sources = core_sql(f"SELECT count(*) FROM media_replicas r JOIN media_objects m ON m.id=r.media_object_id "
                               f"WHERE m.public_id='{video['id']}' AND m.state='ACTIVE'")
            assert sources == "0"
            results.evidence(15, "retained file still byte-identical and playable in Jellyfin; reselection refused; no longer"
                                 " an eligible source")
        item(15, removed)

        # --------------------------------------------- 19 no enumeration
        def enumeration():
            assert request(nodes["a"]["base"] + "/api/v1/global-members")[0] == 404
            federation = request(core_base + f"/api/federation/v1/nodes/{nodes['a']['node_id']}/memberships")[0]
            assert federation == 404, federation
            assert request(core_base + f"/api/federation/v1/nodes/{nodes['a']['node_id']}/media")[0] == 401  # control: route exists
            assert request(core_base + "/api/admin/v1/users", headers=admin)[0] in (404, 405)
            run(["npm", "run", "build"], cwd=ROOT / "node-ui", capture=True, timeout=300)
            bundle = "".join(p.read_text(errors="replace") for p in (ROOT / "node-ui/dist/assets").glob("*.js"))
            assert bundle and "global-members" not in bundle
            for label in ("a", "b"):
                surfaces = "".join(request(nodes[label]["base"] + p)[2].decode(errors="replace")
                                   for p in ("/api/v1/status", "/api/v1/local-users", "/api/v1/media", "/api/v1/diagnostics"))
                assert "alice-e2e" not in surfaces and nodes["user_id"] not in surfaces
            results.evidence(19, "Agent and Core membership listing routes return 404; Core has no user listing; operator API"
                                 " surfaces and UI bundle carry no global username or user ID")
        item(19, enumeration)

        # ------------------------------------------ 20 A cannot access B
        def isolation():
            b_ips = {}
            for service in ("arknet-agent", "postgres", "jellyfin", "arknet-transfer", "arknet-ui"):
                cid = container(lambda *i, **k: node_compose("b", *i, **k), service)
                if not cid:
                    continue
                info = json.loads(run(["docker", "inspect", cid], capture=True))[0]["NetworkSettings"]["Networks"]
                for name, value in info.items():
                    b_ips[(service, name.rsplit("_", 1)[-1] if name.startswith(nodes["b"]["project"]) else name)] = value["IPAddress"]
            ports = {"arknet-agent": 8081, "postgres": 5432, "jellyfin": 8096, "arknet-transfer": 8084, "arknet-ui": 5173}
            probe = ("import socket,sys\nip,port=sys.argv[1],int(sys.argv[2])\ns=socket.socket();s.settimeout(3)\n"
                     "try:\n s.connect((ip,port));print('open')\nexcept Exception:\n print('closed')\n")
            a_transfer = container(lambda *i, **k: node_compose("a", *i, **k), "arknet-transfer")
            a_agent = container(lambda *i, **k: node_compose("a", *i, **k), "arknet-agent")
            reachable = []
            for (service, network), ip in b_ips.items():
                verdict = run(["docker", "exec", a_transfer, "python3", "-c", probe, ip, str(ports[service])], capture=True)
                agent_verdict = run(["docker", "exec", a_agent, "sh", "-c",
                                     f"nc -z -w 3 {ip} {ports[service]} >/dev/null 2>&1 && echo open || echo closed"], capture=True)
                if "open" in (verdict, agent_verdict):
                    reachable.append(f"{service}@{network}")
            # B's transfer service shares the simulated Internet for the peer protocol.
            # Its control API must still refuse Node A without B's local token.
            allowed = {"arknet-transfer@" + wan}
            assert set(reachable) <= allowed, reachable
            if reachable:
                code = run(["docker", "exec", a_transfer, "python3", "-c",
                            "import urllib.request,urllib.error,sys\ntry:\n urllib.request.urlopen('http://%s:8084/api/transfer/v1/transfers'%sys.argv[1],timeout=3);print(200)\n"
                            "except urllib.error.HTTPError as e:\n print(e.code)\n", b_ips[("arknet-transfer", wan)]], capture=True)
                assert code == "401", code
            results.evidence(20, "from Node A's Agent and transfer containers, Node B's Agent, UI, PostgreSQL and Jellyfin are"
                                 " unreachable; only B's peer endpoint is on the shared network and its control API returns 401")
            env = dict(nodes["a"]["env"])
            env["ARKNET_TEST_DATABASE_URL"] = f"postgresql://arknet:{nodes['a']['password']}@postgres:5432/arknet_node?sslmode=disable"
            env.update(ARKNET_TEST_CORE_URL=f"http://host.docker.internal:{core_port}", ARKNET_TEST_TARGET_NODE_ID=nodes["b"]["node_id"])
            run(["docker", "run", "--rm", "--network", nodes["a"]["project"] + "_node", "-e", "ARKNET_TEST_DATABASE_URL",
                 "-e", "ARKNET_TEST_CORE_URL", "-e", "ARKNET_TEST_TARGET_NODE_ID", "-e", "CGO_ENABLED=0", "-e", "GOMAXPROCS=2",
                 "-v", str(ROOT / "scripts/node-enrollment-auth-probe.go") + ":/src/probe/main.go:ro", images["agent-build"],
                 "go", "run", "-trimpath", "-p=1", "./probe"], env=env, timeout=600)
            results.evidence(20, "Node A's federation credential presented for Node B's Core route was rejected (401)")
        item(20, isolation)

        # --------------------------------------------------- 16 revocation
        def revocation():
            decision = request_json(core_base + f"/api/admin/v1/nodes/{nodes['b']['node_id']}/revoke", "POST", headers=admin)[2]
            assert decision["enrollment_status"] == "REVOKED" and decision["trust_status"] == "BLOCKED"
            request(nodes["b"]["base"] + f"/api/v1/media/{later['id']}/selection", "POST", {})
            started = request(nodes["b"]["base"] + f"/api/v1/media/{later['id']}/transfer/start", "POST", {})
            assert started[0] != 200, "revoked Node B started a new authorized transfer"
            assert transfer_status("b", later["id"]) is None
            results.evidence(16, "after revocation Node B could not obtain a new swarm authorization (transfer start refused)")
            wait_until("B REVOKED", lambda: in_state("b", "REVOKED"), 120)
            results.evidence(16, "Node B learned the authoritative revocation and entered terminal REVOKED")
            assert in_state("a", "CONNECTED")
            results.evidence(16, "Node A unaffected (CONNECTED)")
        item(16, revocation)

        # ---------------------------------------------------- secrets
        logs = []
        for project in projects:
            for cid in run(["docker", "ps", "-aq", "--filter", f"label=com.docker.compose.project={project}"], capture=True).split():
                result = subprocess.run(["docker", "logs", cid], text=True, capture_output=True, timeout=30, check=False)
                logs.append(result.stdout + result.stderr)
        leaked = [v for v in known_secrets if v and v in "\n".join(logs)]
        print(("PASS" if not leaked else "FAIL") + f": container logs contain {len(leaked)} known secret values", flush=True)
        if leaked:
            results.failed(19, "known secrets present in container logs")
    except SetupOnly:
        pass
    except Exception as error:
        failure = error
        traceback.print_exc()
        try:
            text = "\n".join(compose("logs", "--tail", "40", capture=True, check=False, timeout=60) for compose in
                             (core_compose, lambda *i, **k: node_compose("a", *i, **k), lambda *i, **k: node_compose("b", *i, **k)))
            for value in sorted((v for v in known_secrets if v), key=len, reverse=True):
                text = text.replace(value, "[REDACTED]")
            print("Sanitized diagnostics:\n" + text.replace(str(temporary), "[TEMPORARY]")[-15000:], flush=True)
        except Exception:
            pass
    finally:
        for label in ("b", "a"):
            node_compose(label, "down", "--volumes", "--remove-orphans", check=False, timeout=240)
        core_compose("down", "--volumes", "--remove-orphans", check=False, timeout=240)
        for network in networks:
            if network in made_networks:
                run(["docker", "network", "rm", network], check=False, timeout=30)
        for image in images.values():
            run(["docker", "image", "rm", "-f", image], check=False, timeout=60)
        shutil.rmtree(temporary, ignore_errors=True)
        leftovers = []
        for project in projects:
            for kind, command in (("container", ["docker", "ps", "-aq", "--filter", f"label=com.docker.compose.project={project}"]),
                                  ("volume", ["docker", "volume", "ls", "-q", "--filter", f"label=com.docker.compose.project={project}"])):
                if run(command, capture=True, check=False, timeout=30):
                    leftovers.append(kind + ":" + project)
        for network in networks:
            if subprocess.run(["docker", "network", "inspect", network], stdout=subprocess.DEVNULL,
                              stderr=subprocess.DEVNULL, check=False).returncode == 0:
                leftovers.append("network:" + network)
        if temporary.exists():
            leftovers.append("directory")
        print(("PASS: exact scoped cleanup" if not leftovers else "ERROR: cleanup leftovers " + ", ".join(leftovers)), flush=True)

    document = {"schema": "arknet.prompt43.mvp-e2e.v1", "run_id": prefix,
                "completed": failure is None, "items": [results.items[n] for n in sorted(results.items)]}
    if args.results:
        Path(args.results).write_text(json.dumps(document, indent=2) + "\n")
    print(json.dumps({n: v["status"] for n, v in results.items.items()}, indent=1), flush=True)
    if args.setup_only:
        if failure is not None:
            raise SystemExit(1)
        return
    if failure is not None or any(v["status"] != "PASS" for v in results.items.values()):
        raise SystemExit(1)


if __name__ == "__main__":
    main()
