#!/usr/bin/env python3
"""Prompt 43 ArkNet MVP end-to-end acceptance run: Core, Node A and Node B.

Topology (every resource uses one random prefix and is removed afterwards):

* ``gateway-a`` / ``gateway-b`` - separate internal transfer networks. Only
  Core, tracker and the TLS relay attach to both; no direct Node-to-Node path.
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
import re
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
import sys  # noqa: E402  (repository root import path for sibling helper modules)
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from scripts import e2e_diagnostics  # noqa: E402
from scripts import operator_login  # noqa: E402
from scripts import global_login  # noqa: E402
from scripts import revocation_meter  # noqa: E402
from scripts import relay_acceptance  # noqa: E402

# Node heartbeat interval for this run. A revoked Node learns of Core's decision
# at its next heartbeat; its transfers must stop within this plus the slack.
HEARTBEAT_SECONDS = 5
REVOCATION_STOP_SLACK_SECONDS = 10
# How long a revoked Node must stay silent. Core's HTTP server ends every
# web-seed response after 30 s and libtorrent waits about 30 s before asking
# again, so a Node that kept its transfer would resume within this window.
REVOCATION_QUIET_SECONDS = 45

# Core's offline window for this run. Short, so item 13 can prove rejection
# after the window in real time (production default and maximum: 720h).
OFFLINE_WINDOW_SECONDS = 180

ITEMS = {
    2: "Enrollment", 3: "Node restart", 4: "Global user", 5: "Local-only user",
    6: "Pairwise node membership", 7: "Jellyfin", 8: "Media selection",
    9: "BitTorrent v2 Core -> Node transfer", 10: "Interrupted transfer/resume",
    11: "Node A -> Node B swarm contribution", 12: "Offline Jellyfin playback",
    13: "Core-bound global authentication", 14: "Core reconnect",
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
    parser.add_argument("--diagnostics-dir",
                        help="where to write the sanitized failure bundle (default: next to --results, else the temp dir)")
    parser.add_argument("--inject-failure", choices=["after-setup"],
                        help="test hook: fail right after SETUP (with a known secret in the error) to exercise diagnostics")
    args = parser.parse_args(argv)

    p25 = runpy.run_path(str(PROMPT25), run_name="arknet_prompt25_reuse")
    p25["verify_prerequisites"]()
    run, request, request_json = p25["run"], p25["request"], p25["request_json"]
    wait_until, jellyfin_setup, free_port = p25["wait_until"], p25["jellyfin_setup"], p25["free_port"]

    results = Results()
    prefix = "arknet-p43-test-" + uuid.uuid4().hex[:12]
    wan, uplink_a, uplink_b = prefix + "-wan", prefix + "-uplink-a", prefix + "-uplink-b"
    gateways = {label: prefix + "-gateway-" + label for label in ("a", "b")}
    networks = (wan, uplink_a, uplink_b, *gateways.values())
    core_project = prefix + "-core"
    images = {"core": prefix + "-core-image", "tracker": prefix + "-tracker-image",
              "agent": prefix + "-agent-image", "transfer": prefix + "-transfer-image",
              "helper": prefix + "-libtorrent", "agent-build": prefix + "-agent-build",
              "relay": prefix + "-relay-image", "capture": prefix + "-capture-image"}
    temporary = Path(tempfile.mkdtemp(prefix="." + prefix + "-", dir=ROOT)); os.chmod(temporary, 0o755)
    made_networks, known_secrets = set(), []
    captures = []
    gateway_ips = {}
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
    core_override = temporary / "core-override.yaml"
    source = temporary / "source"; source.mkdir(mode=0o755)

    nodes = {}
    for label, uplink in (("a", uplink_a), ("b", uplink_b)):
        password, token, jelly_password = secrets.token_hex(32), secrets.token_hex(32), secrets.token_urlsafe(24)
        known_secrets.extend((password, token, jelly_password))
        cfg = {"project": prefix + "-node-" + label, "uplink": uplink, "agent_port": free_port(),
               "member_port": free_port(), "jellyfin_port": free_port(), "password": password, "jelly_password": jelly_password,
               "override": temporary / f"node-{label}-override.yaml",
               "jelly_config": temporary / f"jellyfin-{label}-config", "jelly_cache": temporary / f"jellyfin-{label}-cache"}
        cfg["jelly_config"].mkdir(mode=0o755); cfg["jelly_cache"].mkdir(mode=0o755)
        cfg["env"] = dict(os.environ, POSTGRES_USER="arknet", POSTGRES_DB="arknet_node", POSTGRES_PASSWORD=password,
                          PGSSLMODE="disable", ARKNET_AGENT_PORT=str(cfg["agent_port"]),
                          # Core-bound login: browsers reach Core and this Node's member
                          # listener on loopback; the Agent reaches Core over its uplink.
                          ARKNET_GLOBAL_LOGIN_CALLBACK_URLS=f"http://127.0.0.1:{cfg['member_port']}/login/callback",
                          ARKNET_CORE_LOGIN_URL=core_base,
                          ARKNET_MEMBER_PORT=str(cfg["member_port"]),
                          ARKNET_UI_HTTPS_BIND="127.0.0.1:", ARKNET_TLS_DIR=str(temporary / f"tls-{label}"),
                          JELLYFIN_BIND=f"127.0.0.1:{cfg['jellyfin_port']}", ARKNET_CORE_URL="http://arknet-core:8080",
                          ARKNET_CORE_ALLOW_HTTP="true", ARKNET_HEARTBEAT_INTERVAL=f"{HEARTBEAT_SECONDS}s",
                          ARKNET_MEDIA_STORAGE_ALLOCATION_GIB="1", ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES="",
                          ARKNET_TRANSFER_TOKEN=token, ARKNET_TRANSFER_TRACKER_ORIGIN="http://arknet-tracker:6969",
                          ARKNET_TRANSFER_TRACKER_ALLOW_HTTP="true", ARKNET_TRANSFER_WEB_SEED_ORIGIN="http://arknet-core:8080",
                          ARKNET_TRANSFER_WEB_SEED_ALLOW_HTTP="true", ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT=str(512 * 1024),
                          # Refreshing grants replaces the worker's web-seed URL.
                          # Leave a bounded transfer window between refreshes;
                          # a 5s stress cadence continually tears down HTTP peers.
                          ARKNET_TRANSFER_RECONCILE_INTERVAL="30s", JELLYFIN_API_KEY="")
        cfg["base"] = f"http://127.0.0.1:{cfg['agent_port']}"
        cfg["member_base"] = f"http://127.0.0.1:{cfg['member_port']}"
        cfg["callback"] = cfg["member_base"] + "/login/callback"
        cfg["jelly_base"] = f"http://127.0.0.1:{cfg['jellyfin_port']}"
        nodes[label] = cfg
    projects = (core_project, nodes["a"]["project"], nodes["b"]["project"])
    admin = {"Authorization": "Bearer " + admin_token}

    def core_compose(*items, capture=False, check=True, timeout=600):
        return run(["docker", "compose", "-p", core_project, "-f", str(ROOT / "deploy/core/compose.yaml"),
                    "-f", str(ROOT / "deploy/core/compose.tracker.yaml"), "-f", str(ROOT / "deploy/core/compose.web-seed.yaml"),
                    "-f", str(ROOT / "deploy/core/compose.node-login.yaml"),
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

    def op(label):
        """Operator session headers for a Node (management routes need them)."""
        return nodes[label]["operator"].headers

    def agent(label, path, method="GET", body=None, expected=(200,), headers=None):
        merged = {**(op(label) if "operator" in nodes[label] else {}), **(headers or {})}
        return request_json(nodes[label]["base"] + path, method, body, merged, expected=expected)[2]

    def member(label, path, method="GET", body=None, expected=(200,), headers=None):
        return request_json(nodes[label]["member_base"] + path, method, body, headers, expected=expected)[2]

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

    def addresses(label, service=None):
        services = (service,) if service else ("arknet-agent", "arknet-transfer", "postgres", "jellyfin", "arknet-ui")
        found = set()
        for name in services:
            cid = container(lambda *i, **k: node_compose(label, *i, **k), name)
            if cid:
                info = json.loads(run(["docker", "inspect", cid], capture=True))[0]
                found.update(v["IPAddress"] for v in info["NetworkSettings"]["Networks"].values() if v["IPAddress"])
        return found

    def start_captures(phase):
        active_captures = []
        for label in ("a", "b"):
            for service in ("arknet-agent", "arknet-transfer"):
                cid = container(lambda *i, **k: node_compose(label, *i, **k), service)
                name = f"{prefix}-{phase}-{label}-{service}"
                captures.append(name)  # registered even if tcpdump startup fails
                capture = relay_acceptance.Capture(run, name, cid, images["capture"], temporary)
                counters = capture.firewall_counters() if service == "arknet-transfer" else None
                active_captures.append((label, service, capture, counters))
        return active_captures

    def finish_captures(active_captures, criterion):
        decoded = {}
        for label, service, capture, counters in active_captures:
            blocked = relay_acceptance.assert_no_blocked_attempts(counters, capture.firewall_counters()) if counters is not None else None
            packets = capture.stop()
            own = addresses(label, service) | {"127.0.0.1", "127.0.0.11"}
            verdict = relay_acceptance.inspect_capture(packets, own, addresses("b" if label == "a" else "a"))
            if service == "arknet-transfer":
                assert blocked is not None
                allowed = {(gateway_ips[label][key], port) for key, port in (("relay", 7443), ("tracker", 6969), ("core", 8080))}
                verdict["connections_classified"] = relay_acceptance.assert_no_direct_attempts(packets, own, allowed)
                verdict["firewall_denied_attempts"] = blocked
                if label == "a":
                    verdict["plaintext_peer_bytes"] = relay_acceptance.assert_plaintext_peer(packets)
            decoded[label, service] = packets
            results.evidence(criterion, f"{label}/{service} full namespace pcap (loopback included): " + json.dumps(verdict))
        return decoded

    def disconnect_webseed(label):
        run(["docker", "network", "disconnect", gateways[label], core_id])

    def connect_webseed(label):
        run(["docker", "network", "connect", "--alias", "arknet-core", "--ip", gateway_ips[label]["core"], gateways[label], core_id])

    def seed_on_a(obj):
        agent("a", f"/api/v1/media/{obj['id']}/selection", "POST", {})
        assert agent("a", f"/api/v1/media/{obj['id']}/transfer/start", "POST", {})["mock"] is False
        wait_until("A downloaded relay fixture", lambda: (media_object("a", obj["id"]) or {}).get("local_state") == "AVAILABLE", 600)
        wait_until("A product seed ready", lambda: any(s["object_id"] == obj["id"] and s["state"] == "SEEDING" for s in transfer_seeds("a")), 120)

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

    def make_video(name, seconds, frequency, minimum_bytes=32 << 20):
        path = source / name
        run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30",
             "-f", "lavfi", "-i", f"sine=frequency={frequency}:sample_rate=48000", "-t", str(seconds), "-c:v", "libx264",
             "-preset", "ultrafast", "-b:v", "5M", "-maxrate", "5M", "-bufsize", "10M", "-pix_fmt", "yuv420p",
             "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", "-f", "mp4", str(path)], timeout=300)
        # Keep a real playable MP4, followed by a valid ISO-BMFF free box full
        # of zeros, not random fixture bytes that can look like packed IPs.
        padding = max(8, minimum_bytes - path.stat().st_size)
        with path.open("ab") as output:
            output.write(padding.to_bytes(4, "big") + b"free")
            output.write(bytes(padding - 8))
        run(["ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "json", str(path)], capture=True)
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

    def write_diagnostics(error):
        """Sanitized state for post-mortem, collected while every container still exists."""
        def logs(compose, *services, tail="200"):
            return compose("logs", "--no-color", "--timestamps", "--tail", tail, *services,
                           capture=True, check=False, timeout=60)

        def worker_events(label):
            text = logs(lambda *i, **k: node_compose(label, *i, **k), "arknet-transfer", tail="2000")
            return "\n".join(line for line in text.splitlines() if '"event"' in line) or "<no worker events>"

        collectors = {
            "failure": lambda: e2e_diagnostics.failure_text(error),
            "results": lambda: {n: {"status": v["status"], "evidence": v["evidence"]} for n, v in results.items.items()},
            "core-nodes": lambda: core_sql("SELECT public_id, enrollment_status, trust_status, peer_serving FROM nodes ORDER BY public_id"),
            "core-replicas": lambda: core_sql("SELECT m.public_id, n.public_id FROM media_replicas r JOIN nodes n ON n.id=r.node_id "
                                              "JOIN media_objects m ON m.id=r.media_object_id ORDER BY 1, 2"),
            "core-logs": lambda: logs(core_compose, "arknet-core", "arknet-tracker"),
        }
        for label in nodes_labels:
            compose = (lambda l: lambda *i, **k: node_compose(l, *i, **k))(label)
            collectors.update({
                f"node-{label}-status": lambda l=label: agent(l, "/api/v1/status"),
                f"node-{label}-transfers": lambda l=label: agent(l, "/api/v1/transfers"),
                f"node-{label}-media": lambda l=label: agent(l, "/api/v1/media"),
                f"node-{label}-seeds": lambda l=label: transfer_seeds(l),
                f"node-{label}-worker-events": lambda l=label: worker_events(l),
                f"node-{label}-logs": lambda c=compose: logs(c, "arknet-agent", "arknet-transfer", "jellyfin"),
                f"node-{label}-containers": lambda c=compose: c("ps", "--all", capture=True, check=False, timeout=60),
            })
        directory = Path(args.diagnostics_dir) if args.diagnostics_dir else e2e_diagnostics.default_directory(prefix, args.results)
        try:
            sections = e2e_diagnostics.collect(collectors)
            bundle = e2e_diagnostics.write_bundle(directory, sections, known_secrets, temporary)
            print(f"DIAGNOSTICS: wrote {len(sections)} sanitized sections to {bundle} "
                  f"({e2e_diagnostics.scan(bundle, known_secrets)} known secret values present)", flush=True)
        except Exception as bundle_error:
            print(f"ERROR: diagnostics bundle not written: {type(bundle_error).__name__}", flush=True)

    nodes_labels = ("a", "b")

    try:
        # ---------------------------------------------------------------- setup
        key_helper = temporary / "grant-helper.go"; key_helper.write_text(p25["KEY_HELPER"]); os.chmod(key_helper, 0o600)
        (temporary / "make-torrent.py").write_text(p25["MAKE_TORRENT"])
        run(["go", "run", str(key_helper), "keygen", str(temporary)], timeout=120)
        private_key, public_key = temporary / "tracker-private.key", temporary / "tracker-public.key"
        os.chmod(private_key, 0o644)  # disposable key; Core's non-root UID reads the bind mount
        core_env.update(ARKNET_TRACKER_SIGNING_KEY_FILE=str(private_key), ARKNET_TRACKER_VERIFY_KEY_FILE=str(public_key),
                        ARKNET_CORE_WEB_SEED_SOURCE=str(source))
        # A dedicated, disposable assertion-signing key (never the tracker key).
        assertion_dir = temporary / "assertion-key"; assertion_dir.mkdir(mode=0o755)
        run(["go", "run", str(key_helper), "keygen", str(assertion_dir)], timeout=120)
        assertion_key, published_keys = assertion_dir / "tracker-private.key", temporary / "assertion-published.keys"
        published_keys.write_text(""); os.chmod(published_keys, 0o644); os.chmod(assertion_key, 0o644)
        known_secrets.extend((assertion_key.read_text().strip(), private_key.read_text().strip()))
        core_env.update(ARKNET_ASSERTION_SIGNING_KEY_FILE=str(assertion_key), ARKNET_ASSERTION_PUBLISHED_KEYS_FILE=str(published_keys),
                        ARKNET_ASSERTION_OFFLINE_WINDOW=f"{OFFLINE_WINDOW_SECONDS}s")
        verify_bind_sources(run, "postgres:18-alpine", (("file", private_key), ("file", public_key), ("directory", source),
                                                        ("file", assertion_key), ("file", published_keys)))
        print("PREFLIGHT: Docker sees the tracker key files and web-seed directory", flush=True)
        run(["docker", "build", "-t", images["helper"], "-"], input_text=p25["LIBTORRENT_DOCKERFILE"], timeout=900)
        run(["docker", "build", "--target", "build", "-t", images["agent-build"], str(ROOT / "node-agent")], timeout=900)
        # Dedicated keys: list, per-relay revocation, endpoint attestation.
        relay_dir = temporary / "relay"; relay_dir.mkdir(mode=0o755)
        for purpose in ("list", "revocation", "endpoint"):
            directory = relay_dir / purpose; directory.mkdir(mode=0o755)
            run(["go", "run", str(key_helper), "keygen", str(directory)], timeout=120)
            os.chmod(directory / "tracker-private.key", 0o644)
            known_secrets.append((directory / "tracker-private.key").read_text().strip())
        relay_cert, relay_tls_key = relay_dir / "cert.pem", relay_dir / "tls.key"
        run(["openssl", "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(relay_tls_key)])
        run(["openssl", "req", "-new", "-x509", "-key", str(relay_tls_key), "-days", "2",
             "-subj", "/CN=arknet-relay", "-out", str(relay_cert)])
        os.chmod(relay_tls_key, 0o644)  # disposable fixture, readable by relay UID
        known_secrets.append(relay_tls_key.read_text())
        pub_pem = subprocess.run(["openssl", "x509", "-in", str(relay_cert), "-pubkey", "-noout"],
                                 capture_output=True, check=True).stdout
        pub_der = subprocess.run(["openssl", "pkey", "-pubin", "-outform", "DER"], input=pub_pem,
                                 capture_output=True, check=True).stdout
        pin = hashlib.sha256(pub_der).hexdigest()
        (relay_dir / "relays.json").write_text(json.dumps([{
            "id": "acceptance", "address": "arknet-relay:7443", "spki_sha256": pin,
            "control_url": "http://arknet-relay:7080",
            "revocation_key_file": "/run/relay/revocation/tracker-private.key"}]))
        core_env["ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE"] = str(relay_dir / "endpoint/tracker-public.key")
        for cfg in nodes.values():
            cfg["env"]["ARKNET_RELAY_LIST_VERIFY_KEY"] = (relay_dir / "list/tracker-public.key").read_text().strip()
        run(["docker", "build", "-t", images["relay"], str(ROOT / "relay")], timeout=900)
        run(["docker", "build", "-t", images["capture"], "-"], input_text=relay_acceptance.CAPTURE_DOCKERFILE, timeout=900)
        import ipaddress
        for network in networks:
            flags = ["--internal"] if network in gateways.values() else []
            run(["docker", "network", "create", *flags, network], capture=True); made_networks.add(network)
            if network in gateways.values():
                subnet = json.loads(run(["docker", "network", "inspect", network], capture=True))[0]["IPAM"]["Config"][0]["Subnet"]
                base = ipaddress.ip_network(subnet).network_address
                label = next(k for k, v in gateways.items() if v == network)
                gateway_ips[label] = {k: str(base + n) for k, n in (("relay", 10), ("core", 11), ("tracker", 12), ("transfer", 20))}
        external = {"gateway-" + label: {"external": True, "name": network} for label, network in gateways.items()}
        relay_environment = {
            "ARKNET_RELAY_ID": "acceptance", "ARKNET_RELAY_BIND_ADDR": "0.0.0.0:7443",
            "ARKNET_RELAY_CONTROL_BIND_ADDR": "0.0.0.0:7080",
            "ARKNET_RELAY_GRANT_VERIFY_KEY_FILE": "/run/grant-public.key",
            "ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE": "/run/relay/revocation/tracker-public.key",
            "ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE": "/run/relay/endpoint/tracker-private.key",
            "ARKNET_RELAY_TLS_CERT_FILE": "/run/relay/cert.pem", "ARKNET_RELAY_TLS_KEY_FILE": "/run/relay/tls.key"}
        core_override.write_text(relay_acceptance.compose_override({"services": {
            "arknet-core": {"image": images["core"], "networks": {"core": {}, "wan": {},
                **{"gateway-"+l: {"ipv4_address": gateway_ips[l]["core"]} for l in gateways}},
                "environment": {"ARKNET_RELAYS_FILE": "/run/relay/relays.json",
                                "ARKNET_RELAY_LIST_SIGNING_KEY_FILE": "/run/relay/list/tracker-private.key"},
                "volumes": [f"{relay_dir}:/run/relay:ro"]},
            "arknet-tracker": {"image": images["tracker"], "networks": {"tracker-edge": {}, "wan": {},
                **{"gateway-"+l: {"ipv4_address": gateway_ips[l]["tracker"]} for l in gateways}}},
            "arknet-relay": {"image": images["relay"], "environment": relay_environment,
                "networks": {"wan": {}, **{"gateway-"+l: {"ipv4_address": gateway_ips[l]["relay"]} for l in gateways}},
                "volumes": [f"{relay_dir}:/run/relay:ro", f"{public_key}:/run/grant-public.key:ro"],
                "read_only": True, "cap_drop": ["ALL"], "security_opt": ["no-new-privileges:true"]}},
            "networks": {"wan": {"external": True, "name": wan}, **external}}))
        for label, cfg in nodes.items():
            cfg["override"].write_text(relay_acceptance.compose_override({"services": {
                "arknet-agent": {"image": images["agent"], "networks": ["node", "media-services", "transfer-control", "uplink"]},
                "arknet-transfer": {"image": images["transfer"], "networks": {
                    "transfer-control": {}, "gateway": {"ipv4_address": gateway_ips[label]["transfer"]}},
                    "cap_add": ["NET_ADMIN", "SETUID", "SETGID", "SETPCAP"],
                    "extra_hosts": relay_acceptance.gateway_hosts(gateway_ips[label]),
                    "environment": {"ARKNET_TRANSFER_RELAY_EGRESS": "arknet-relay:7443",
                                    "ARKNET_TRANSFER_LISTEN_INTERFACES": "127.0.0.1:6881"},
                    "sysctls": {"net.ipv6.conf.all.disable_ipv6": "1"}},
                "jellyfin": {"image": "jellyfin/jellyfin:10.10.7", "volumes": [
                    f"{cfg['jelly_config']}:/config", f"{cfg['jelly_cache']}:/cache", "media:/srv/arknet/media:ro"]}},
                "networks": {"uplink": {"external": True, "name": cfg["uplink"]},
                             "gateway": {"external": True, "name": gateways[label]}}}))
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
            # Management routes require an operator (Prompt 45B): sign in the way an operator does.
            code = operator_login.setup_code(node_compose(label, "exec", "-T", "arknet-agent", "arknet-agent", "operator",
                                                          "setup-code", capture=True))
            known_secrets.extend((code, code.replace("-", "")))
            cfg["operator"] = operator_login.bootstrap(cfg["base"], code, request, username=f"operator-{label}",
                                                       member_base=cfg["member_base"])
            known_secrets.extend(cfg["operator"].secrets)
        print("SETUP: Core, tracker, web seed, Node A and Node B are running", flush=True)
        if args.inject_failure == "after-setup":
            # The message deliberately carries a known secret: the bundle must redact it.
            raise RuntimeError("injected harness failure after setup (admin token " + admin_token + ")")
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
            member("a", "/api/v1/local-users/sessions", "POST", {"username": "local-bob", "password": local_password}, (201,))
            other = request(nodes["b"]["member_base"] + "/api/v1/local-users/sessions", "POST",
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
        video = make_video("media_" + secrets.token_hex(32), 2, 880)
        later = make_video("media_" + secrets.token_hex(32), 2, 440, 64 << 20)
        # Zero-filled free-box padding leaves a bounded interruption window.
        active = make_video("media_" + secrets.token_hex(32), 2, 550, 96 << 20)

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
            assert request(nodes["a"]["member_base"] + "/api/v1/local-users/sessions", "POST",
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
            request_json(nodes["a"]["base"] + f"/api/v1/media/{video['id']}/transfer/resume", "POST", {}, op("a"), expected=(200, 409))
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

        # ------------------------------ 13a Core-bound online login (ARKSEC-001)
        def callbacks_registered(label):
            return core_sql("SELECT count(*) FROM node_login_callbacks c JOIN nodes n ON n.id=c.node_id "
                            f"WHERE n.public_id='{nodes[label]['node_id']}'") == "1"

        def node_login(label, browser, base=None):
            session, assertion = global_login.login(browser, base or nodes[label]["member_base"], nodes[label]["callback"],
                                                    "alice-e2e", global_password)
            known_secrets.extend((session["token"], assertion, *browser.cookie_values()))
            return session, assertion

        def online_global_login():
            for label in ("a", "b"):
                wait_until(label + " login callback registered at Core", lambda l=label: callbacks_registered(l), 120)
            # The password route is gone from the Node (ARKSEC-001).
            assert request(nodes["a"]["member_base"] + "/api/v1/global-users/sessions", "POST",
                           {"member_id": nodes["a"]["member_id"], "password": global_password})[0] == 410
            browser = global_login.Browser()
            session, assertion = node_login("a", browser)
            nodes["login_issued_at"] = time.time()
            assert session["member_id"] == nodes["a"]["member_id"] and session["offline"] is False
            assert request(nodes["a"]["member_base"] + "/api/v1/global-users/session",
                           headers={"Authorization": "Bearer " + session["token"]})[0] == 200
            nodes["member_token"], nodes["member_browser"] = session["token"], browser
            # A global-member session never confers operator rights (ARKSEC-002).
            assert request(nodes["a"]["base"] + "/api/v1/status", headers={"Authorization": "Bearer " + session["token"]})[0] == 401
            results.evidence(13, "online login: the password was typed only on Core's page; Core redirected to Node A's"
                                 " registered callback; Node A verified the one-time assertion and created a session")
            # Replay: the same assertion again, in the same browser and in a fresh login.
            assert global_login.complete(browser, nodes["a"]["member_base"], assertion)[0] == 401
            other = global_login.Browser()
            assert global_login.start(other, nodes["a"]["member_base"], nodes["a"]["callback"])[0] == 200
            assert global_login.complete(other, nodes["a"]["member_base"], assertion)[0] == 401
            # Cross-Node: Node A's assertion presented to Node B in a login Node B started.
            at_b = global_login.Browser()
            assert global_login.start(at_b, nodes["b"]["member_base"], nodes["b"]["callback"])[0] == 200
            assert global_login.complete(at_b, nodes["b"]["member_base"], assertion)[0] == 401
            results.evidence(13, "replayed assertion rejected by Node A (same and new login); Node A's assertion rejected"
                                 " by Node B")

            # Malicious Node: record every byte Node A's Agent receives during a login.
            relay = global_login.RecordingRelay("127.0.0.1", nodes["a"]["member_port"])
            try:
                node_login("a", global_login.Browser(), relay.base)
            finally:
                relay.close()
            captured = relay.candidates()
            joined = "\n".join(captured)
            assert captured and global_password not in joined and nodes["core_session"] not in joined
            for value in captured:
                assert request(core_base + "/api/identity/v1/sessions", "POST",
                               {"username": "alice-e2e", "password": value})[0] == 401, "captured value is a Core password"
                assert request(core_base + "/api/identity/v1/session", headers={"Authorization": "Bearer " + value})[0] == 401
                assert request(nodes["b"]["member_base"] + "/api/v1/global-users/session",
                               headers={"Authorization": "Bearer " + value})[0] == 401
                probe = global_login.Browser()
                assert global_login.start(probe, nodes["b"]["member_base"], nodes["b"]["callback"])[0] == 200
                assert global_login.complete(probe, nodes["b"]["member_base"], value)[0] == 401
            # Node A's recorded login-state cookie plus its assertion, replayed at Node B.
            recorded = bytes(relay.received).decode("latin-1")
            stolen_cookies = dict(re.findall(r"(__Host-arknet_global_login)=([A-Za-z0-9_-]+)", recorded))
            assert stolen_cookies, "the harness did not see the browser's login-state cookie"
            stolen = global_login.Browser()
            stolen.cookies[nodes["b"]["member_base"]] = stolen_cookies
            for value in captured:
                assert global_login.complete(stolen, nodes["b"]["member_base"], value)[0] == 401
            results.evidence(13, f"malicious-Node harness: {len(relay.received)} bytes received by Node A's Agent during a"
                                 f" login; none of {len(captured)} captured values contains the password or Core session,"
                                 " signs in at Core (password or bearer), or opens a session at Node B (assertion or bearer)")
        item(13, online_global_login)

        # ------------------------- 12 offline playback, 13b offline auth, 14
        def offline():
            core_compose("stop", "arknet-core", "arknet-tracker")
            wait_until("A offline", lambda: in_state("a", "DEGRADED", "OFFLINE"), 120)
            results.evidence(14, "Core outage moved Node A to " + status("a")["state"] + " while the Agent kept serving")
        item(14, offline)

        def offline_auth():
            browser = nodes.get("member_browser")
            assert browser is not None, "online login (13a) did not run"
            remaining = nodes["login_issued_at"] + OFFLINE_WINDOW_SECONDS - time.time()
            assert remaining > 15, f"too little of the offline window left ({remaining:.0f}s)"
            code, session = global_login.start(browser, nodes["a"]["member_base"], nodes["a"]["callback"])
            assert code == 201 and session["offline"] is True and session["member_id"] == nodes["a"]["member_id"], code
            known_secrets.append(session["token"]); nodes["offline_token"] = session["token"]
            assert global_login.start(global_login.Browser(), nodes["a"]["member_base"], nodes["a"]["callback"])[0] == 503
            # Node A issued this device credential, so a malicious Node A has it: it is useless at Node B.
            stolen = global_login.Browser()
            stolen.cookies[nodes["b"]["member_base"]] = dict(browser.cookies[nodes["a"]["member_base"]])
            code_b, refused = global_login.start(stolen, nodes["b"]["member_base"], nodes["b"]["callback"])
            assert code_b == 401 and refused["error"] == "offline_login_rejected", code_b
            local = request(nodes["a"]["member_base"] + "/api/v1/local-users/sessions", "POST", {"username": "local-bob", "password": local_password})
            assert local[0] == 201
            results.evidence(13, "Core offline: the browser's device credential and Node A's Core-signed grant gave an offline"
                                 " session inside the window; a browser without a device credential was refused; Node A's"
                                 " device credential was refused by Node B; local user still authenticates")
        item(13, offline_auth)

        def offline_playback():
            assert jellyfin_stream("a", video["sha256"], video["size"])
            results.evidence(12, "with Core and tracker stopped, Jellyfin on Node A streamed the exact completed MP4")
        item(12, offline_playback)

        def after_window():
            time.sleep(max(0, nodes["login_issued_at"] + OFFLINE_WINDOW_SECONDS + 10 - time.time()))
            code, body = global_login.start(nodes["member_browser"], nodes["a"]["member_base"], nodes["a"]["callback"])
            assert code == 401 and body["error"] == "offline_login_rejected", code
            assert request(nodes["a"]["member_base"] + "/api/v1/global-users/session",
                           headers={"Authorization": "Bearer " + nodes["offline_token"]})[0] == 401
            results.evidence(13, f"after the {OFFLINE_WINDOW_SECONDS}s offline window, offline login was rejected and the"
                                 " offline session had ended")
        item(13, after_window)

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
            phase_captures = start_captures("swarm")
            agent("b", f"/api/v1/media/{video['id']}/selection", "POST", {})
            started = agent("b", f"/api/v1/media/{video['id']}/transfer/start", "POST", {})
            assert started["mock"] is False
            wait_until("B metadata", lambda: (transfer_status("b", video["id"]) or {}).get("state") == "DOWNLOADING", 60)
            before_disconnect = transfer_status("b", video["id"])["completed_bytes"]
            assert 0 <= before_disconnect < video["size"]
            disconnect_webseed("b")
            try:
                done = wait_until("B AVAILABLE from Node A through relay", lambda: (lambda s: s if s and s["completed_bytes"] == video["size"] else None)(
                    transfer_status("b", video["id"])), 600)
            finally:
                connect_webseed("b")
            decoded = finish_captures(phase_captures, 11)
            a_peer_bytes = sum(len(p['payload']) for p in decoded['a', 'arknet-transfer']
                               if p['src'] == '127.0.0.1' and p['sport'] == 6881)
            assert a_peer_bytes >= 1 << 20, "no real libtorrent peer contribution captured on A loopback"
            assert done["state"] == "COMPLETED"
            wait_until("B AVAILABLE", lambda: (media_object("b", video["id"]) or {}).get("local_state") == "AVAILABLE", 180)
            b_hash = run(["docker", "run", "--rm", "-v", nodes["b"]["project"] + "_media:/m:ro", images["helper"], "-c",
                          f"import hashlib,pathlib;print(hashlib.sha256(pathlib.Path('/m/{video['id']}.mp4').read_bytes()).hexdigest())"],
                         capture=True)
            assert b_hash == video["sha256"]
            results.evidence(11, f"relay-only private v2 peer contribution: {a_peer_bytes} loopback bytes from A's libtorrent;"
                                 " B completed exact SHA-256 with its Core web seed disconnected")

            # all relays down: first prove active peer flow without an origin,
            # then a long quiet interval, then origin-only completion with the
            # relay still stopped. Capture every attempted connection throughout.
            seed_on_a(later)
            outage_captures = start_captures("outage")
            agent("b", f"/api/v1/media/{later['id']}/selection", "POST", {})
            assert agent("b", f"/api/v1/media/{later['id']}/transfer/start", "POST", {})["mock"] is False
            wait_until("B outage metadata", lambda: transfer_status("b", later["id"]), 60)
            disconnect_webseed("b")
            b_transfer = container(lambda *i, **k: node_compose("b", *i, **k), "arknet-transfer")
            sampler = revocation_meter.RxSampler(b_transfer, 300)
            try:
                wait_until("positive relay-only flow before outage", lambda: revocation_meter.recent_rate(sampler.samples) >= 128 * 1024, 120, .25)
                assert transfer_status("b", later["id"])["completed_bytes"] < later["size"] - (10 << 20)
                stopped_at = time.time()
                core_compose("stop", "arknet-relay")
                time.sleep(REVOCATION_QUIET_SECONDS + 8)
                outage_verdict = revocation_meter.analyze(sampler.stop(), stopped_at, 5, REVOCATION_QUIET_SECONDS)
            except Exception:
                core_compose("start", "arknet-relay")
                raise
            finally:
                sampler.stop()
                connect_webseed("b")
            try:
                wait_until("webseed continues with all relays down", lambda: (media_object("b", later["id"]) or {}).get("local_state") == "AVAILABLE", 600)
                outage = finish_captures(outage_captures, 11)
                packets = outage['b', 'arknet-transfer']
                origin_bytes = sum(len(p['payload']) for p in packets if p['src'] == gateway_ips['b']['core']
                                   and p['sport'] == 8080 and p['time'] > stopped_at + 8)
                assert origin_bytes >= 1 << 20, "webseed did not carry payload while relays were down"
                late_peer_bytes = sum(len(p['payload']) for p in outage['a', 'arknet-transfer']
                                      if p['src'] == '127.0.0.1' and p['sport'] == 6881 and p['time'] > stopped_at + 8)
                assert late_peer_bytes == 0, "peer bytes continued with every relay stopped"
                results.evidence(11, "all relays down: " + json.dumps(outage_verdict) +
                                 f"; {origin_bytes} origin bytes, {late_peer_bytes} late peer bytes; all outgoing attempts gateway-classified")
            finally:
                core_compose("start", "arknet-relay")
        item(11, swarm)

        # ----------------------------------------------- 15 removed media
        def removed():
            removed_object = request_json(core_base + f"/api/admin/v1/media/{video['id']}/remove", "POST", headers=admin)[2]
            assert removed_object["state"] == "REMOVED"
            retained = wait_until("A REMOVED_RETAINED", lambda: (lambda o: o if o and o["local_state"] == "REMOVED_RETAINED" else None)(
                media_object("a", video["id"])), 120)
            results.evidence(15, "Core tombstone propagated; Node A state " + retained["local_state"])
            assert jellyfin_stream("a", video["sha256"], video["size"])
            again = request(nodes["a"]["base"] + f"/api/v1/media/{video['id']}/selection", "POST", {}, op("a"))
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
                surfaces = "".join(request(nodes[label]["base"] + p, headers=op(label))[2].decode(errors="replace")
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
            assert not reachable, reachable
            results.evidence(20, "from Node A's Agent and transfer containers, every Node B address is unreachable,"
                                 " including its transfer-control API; separate internal gateway networks expose no direct peer path")
            env = dict(nodes["a"]["env"])
            env["ARKNET_TEST_DATABASE_URL"] = f"postgresql://arknet:{nodes['a']['password']}@postgres:5432/arknet_node?sslmode=disable"
            env.update(ARKNET_TEST_CORE_URL=f"http://host.docker.internal:{core_port}", ARKNET_TEST_TARGET_NODE_ID=nodes["b"]["node_id"])
            run(["docker", "run", "--rm", "--network", nodes["a"]["project"] + "_node", "-e", "ARKNET_TEST_DATABASE_URL",
                 "-e", "ARKNET_TEST_CORE_URL", "-e", "ARKNET_TEST_TARGET_NODE_ID", "-e", "CGO_ENABLED=0", "-e", "GOMAXPROCS=2",
                 "-v", str(ROOT / "scripts/node-enrollment-auth-probe.go") + ":/src/probe/main.go:ro", images["agent-build"],
                 "go", "run", "-trimpath", "-p=1", "./probe"], env=env, timeout=600)
            results.evidence(20, "Node A's federation credential presented for Node B's Core route was rejected (401)")
            # Node B's management address refuses callers without B's operator session (ARKSEC-002).
            denied = []
            for method, path, body in (("GET", "/api/v1/status", None), ("GET", "/api/v1/local-users", None),
                                       ("POST", "/api/v1/invitations", {}), ("PUT", "/api/v1/settings/peer-serving", {"enabled": True}),
                                       ("POST", "/api/v1/updates/check", {})):
                callers = [("anonymous", None), ("Node A operator", op("a"))]
                if nodes.get("member_token"):
                    callers.append(("global member bearer", {"Authorization": "Bearer " + nodes["member_token"]}))
                for caller, headers in callers:
                    status = request(nodes["b"]["base"] + path, method, body, headers)[0]
                    if status not in (401, 403):
                        denied.append(f"{method} {path} as {caller}: {status}")
            assert not denied, denied
            assert agent("b", "/api/v1/settings/peer-serving") == {"enabled": False}, "an unauthenticated write changed Node B"
            assert request(nodes["b"]["member_base"] + "/api/v1/status")[0] == 404
            assert request(nodes["b"]["base"] + "/api/v1/global-users/sessions", "POST", {})[0] == 404
            assert request(nodes["b"]["member_base"] + "/health")[0] == 200
            results.evidence(20, "Node B's management address returned 401 to anonymous callers, to Node A's operator"
                                 " session and to a member bearer token on status, users, invitations, settings and updates")
            results.evidence(20, "Node B's member listener returned 404 for management status; its management listener returned"
                                 " 404 for member login; health remained available only on the member listener")
        item(20, isolation)

        # --------------------------------------------------- 16 revocation
        def revocation():
            seed_on_a(active)
            phase_captures = start_captures("revocation")
            agent("b", f"/api/v1/media/{active['id']}/selection", "POST", {})
            assert agent("b", f"/api/v1/media/{active['id']}/transfer/start", "POST", {})["mock"] is False
            wait_until("B receiving", lambda: (transfer_status("b", active["id"]) or {}).get("completed_bytes", 0) >= 1 << 20, 120, .25)
            disconnect_webseed("b")
            b_transfer = container(lambda *i, **k: node_compose("b", *i, **k), "arknet-transfer")
            b_agent = container(lambda *i, **k: node_compose("b", *i, **k), "arknet-agent")
            sampler = revocation_meter.RxSampler(b_transfer, 300)
            frozen = False
            try:
                wait_until("positive relay-only bytes", lambda: revocation_meter.recent_rate(sampler.samples) >= 128 * 1024, 120, .25)
                progress = transfer_status("b", active["id"])["completed_bytes"]
                assert active["size"] - progress >= 10 << 20, "too little left to observe a stop"
                # Non-cooperative negative control: the Agent cannot cancel or
                # renew anything while frozen. Only authoritative relay push
                # can terminate the live flow before its five-minute expiry.
                run(["docker", "pause", b_agent], capture=True)
                frozen = True
                time.sleep(3)
                # Relayed flow is bursty: wait, with the Agent still frozen, until
                # it is clearly positive immediately before the revocation.
                wait_until("relay-only flow with B's Agent frozen",
                           lambda: revocation_meter.recent_rate(sampler.samples) >= 128 * 1024, 30, .25)
                revoked_at = time.time()
                revoked_monotonic = time.monotonic()
                decision = request_json(core_base + f"/api/admin/v1/nodes/{nodes['b']['node_id']}/revoke", "POST", headers=admin)[2]
                assert decision["enrollment_status"] == "REVOKED" and decision["trust_status"] == "BLOCKED"

                def events(service):
                    text = core_compose("logs", "--no-color", "--no-log-prefix", "--since", str(revoked_at), service, capture=True)
                    entries = []
                    for line in text.splitlines():
                        try:
                            entries.append(json.loads(line))
                        except ValueError:
                            continue
                    return entries

                observed, delivered, elapsed = [], [], float('inf')
                closed = 0
                while time.monotonic() - revoked_monotonic <= 5:
                    applied = [e for e in events("arknet-relay") if e.get("msg") == "relay_revocation_received"]
                    delivered = [e for e in events("arknet-core") if e.get("msg") == "relay_revocation_delivered" and e.get("relay_id") == "acceptance"]
                    closed = sum(e.get("connections_closed", 0) for e in applied)
                    elapsed = time.monotonic() - revoked_monotonic
                    observed.append({"after_seconds": round(elapsed, 3), "core_deliveries": len(delivered),
                                     "relay_pushes": len(applied), "connections_closed": closed})
                    if delivered and applied and closed >= 2:
                        break
                    time.sleep(.2)
                assert closed >= 2 and delivered and elapsed <= 5, "Core push did not close a live relay session within 5s"
                # Still frozen throughout the quiet window: cooperative Agent
                # cancellation cannot manufacture a relay enforcement PASS.
                time.sleep(REVOCATION_QUIET_SECONDS + 6)
                verdict = revocation_meter.analyze(sampler.stop(), revoked_at, 5, REVOCATION_QUIET_SECONDS)
                results.evidence(16, "Core push / relay live-session closure while B Agent frozen: " + json.dumps(observed))
                results.evidence(16, "relay-only revocation byte stop: " + json.dumps(verdict))
            finally:
                sampler.stop()
                if frozen:
                    run(["docker", "unpause", b_agent], capture=True, check=False)
                connect_webseed("b")
            decoded = finish_captures(phase_captures, 16)
            packets = decoded['b', 'arknet-transfer']
            socket_verdict = relay_acceptance.assert_relay_revoked(observed, packets, gateway_ips['b']['relay'], revoked_at)
            a_peer_before = sum(len(p['payload']) for p in decoded['a', 'arknet-transfer']
                                if p['src'] == '127.0.0.1' and p['sport'] == 6881 and revoked_at - 3 <= p['time'] <= revoked_at)
            assert a_peer_before >= 128 * 1024, "no positive real A peer stream at revoke"
            results.evidence(16, f"pcap: {a_peer_before} real A peer bytes in last 3s; " + json.dumps(socket_verdict))
            wait_until("B REVOKED", lambda: in_state("b", "REVOKED"), 120, .25)
            cancelled = wait_until("B transfer CANCELLED", lambda: (lambda t: t if t and t["state"] == "CANCELLED" else None)(
                transfer_status("b", active["id"])), 60, .25)
            assert cancelled["completed_bytes"] < active["size"]
            assert (media_object("b", active["id"]) or {}).get("local_state") == "ERROR"
            refused = request(nodes["b"]["base"] + f"/api/v1/media/{active['id']}/transfer/start", "POST", {}, op("b"))
            assert refused[0] == 409 and b"node_revoked" in refused[2], refused[0]
            b_hash = run(["docker", "run", "--rm", "-v", nodes["b"]["project"] + "_media:/m:ro", images["helper"], "-c",
                          f"import hashlib,pathlib;print(hashlib.sha256(pathlib.Path('/m/{video['id']}.mp4').read_bytes()).hexdigest())"],
                         capture=True)
            assert b_hash == video["sha256"]
            results.evidence(16, "after unfreeze B converged to REVOKED/CANCELLED, refuses new transfers (409 node_revoked), retained media exact")
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
        print(e2e_diagnostics.redact(e2e_diagnostics.failure_text(error), known_secrets, temporary), flush=True)
    finally:
        # Collect before cleanup: an item FAIL never raises, so check results too.
        if failure is not None or any(v["status"] == "FAIL" for v in results.items.values()):
            write_diagnostics(failure)
        for capture_name in captures:
            run(["docker", "rm", "-f", "-v", capture_name], capture=True, check=False, timeout=30)
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
