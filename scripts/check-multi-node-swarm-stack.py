#!/usr/bin/env python3
"""Disposable Prompt 43F Core + two-Node product swarm proof.

The harness reuses Prompt 25 helpers, creates only randomly prefixed resources,
measures web-seed payload (not metadata), exercises Node A's real seed
reconciler and transfer worker, validates hard evidence, and verifies exact
cleanup. It never reads developer environment files or prints grant URLs.

Peer payload uses the product's only peer transport, the private relay
(Prompt 48D): Core runs the shipped ``compose.relay.yaml`` overlay, Nodes get
the signed relay list and ``ARKNET_TRANSFER_RELAY_EGRESS``, and each Node's
transfer sidecar sits on its own isolated network shared only with Core, the
tracker, the web-seed meter and the relay. There is no Node-to-Node path.
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
import sys
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parent.parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from scripts.core_admin import ScopedAdmin  # noqa: E402
PROMPT25 = Path(__file__).with_name("check-core-to-node-transfer-stack.py")
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from scripts.check_multi_node_result import result_document
from scripts import e2e_diagnostics
from scripts import harness_scratch
from scripts import operator_login
from scripts import relay_acceptance


METER_PROXY = r'''
import json, os, threading, time, urllib.error, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit
upstream = os.environ["UPSTREAM"].rstrip("/")
rate = int(os.environ.get("RATE_BYTES_PER_SECOND", "0"))
lock = threading.Lock(); throttle = threading.Lock()
counters = {"payload_bytes": 0}; next_send = [time.monotonic()]
class Handler(BaseHTTPRequestHandler):
 # Match Core's Go server: HTTP/1.1 keep-alive with exact Content-Length.
 protocol_version = "HTTP/1.1"
 def log_message(self, format, *args): pass
 def event(self, **value):
  # Sanitized: never logs the capability path, only status and byte counts.
  print(json.dumps(value, separators=(",", ":")), flush=True)
 def do_POST(self):
  if self.path == "/__reset":
   with lock: counters["payload_bytes"] = 0
   self.send_response(204); self.send_header("Content-Length", "0"); self.end_headers(); return
  self.send_error(404)
 def do_GET(self):
  if self.path == "/__stats":
   with lock: body = json.dumps(counters, separators=(",", ":")).encode()
   self.send_response(200); self.send_header("Content-Type", "application/json")
   self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body); return
  request = urllib.request.Request(upstream + self.path)
  for name in ("Range", "Accept"):
   if self.headers.get(name): request.add_header(name, self.headers[name])
  try: response = urllib.request.urlopen(request, timeout=30)
  except urllib.error.HTTPError as error: response = error
  body_kind = "web-seed" if self.path.startswith("/api/web-seed/") else "other"
  self.send_response(response.status)
  for name, value in response.headers.items():
   if name.lower() not in ("connection", "transfer-encoding", "date", "server", "keep-alive"): self.send_header(name, value)
  self.end_headers()
  measure = self.path.startswith("/api/web-seed/") and 200 <= response.status < 300
  sent = 0; failure = ""
  try:
   while True:
    chunk = response.read(32768)
    if not chunk: break
    if measure and rate:
     with throttle:
      now = time.monotonic(); wait = max(0, next_send[0] - now)
      time.sleep(wait)
      next_send[0] = max(now, next_send[0]) + len(chunk) / rate
    self.wfile.write(chunk); self.wfile.flush(); sent += len(chunk)
    if measure:
     with lock: counters["payload_bytes"] += len(chunk)
  except (BrokenPipeError, ConnectionResetError) as error:
   failure = type(error).__name__; self.close_connection = True
  finally:
   response.close()
   shape = "/".join("<id>" if part.startswith("media_") else "<grant>" if "." in part and len(part) > 40
                    else part[:24] for part in urlsplit(self.path).path.split("/"))
   self.event(kind=body_kind, status=response.status, ranged=bool(self.headers.get("Range")), sent=sent,
              failure=failure, shape=shape)
server = ThreadingHTTPServer(("0.0.0.0", 8088), Handler)
print(json.dumps({"ready": True}), flush=True); server.serve_forever()
'''

def load_prompt25():
    if not PROMPT25.is_file():
        raise RuntimeError("Prompt 25 harness is unavailable")
    return runpy.run_path(str(PROMPT25), run_name="arknet_prompt25_reuse")


def write_result(path, value):
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temporary = target.with_name("." + target.name + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    os.replace(temporary, target)


RELAY_ID = "acceptance"
RELAY_EGRESS = "arknet-relay:7443"
RELAY_CONTROL_URL = "http://arknet-relay:7080"
# Core's relay config directory is mounted here by compose.relay.yaml.
CORE_RELAY_MOUNT = "/run/config/arknet-relays"
CORE_COMPOSE_FILES = tuple(Path("deploy/core") / name for name in (
    "compose.yaml", "compose.tracker.yaml", "compose.web-seed.yaml", "compose.relay.yaml"))


def relay_layout(temporary):
    """Disposable relay key/cert paths. Core's mounted directory holds only
    Core-side material (relay list, list signing key, revocation keys); the
    relay's endpoint-signing and TLS private keys live in a separate directory
    that only the relay mounts."""
    core_config, relay_secrets, list_dir = temporary / "relay-core", temporary / "relay-secrets", temporary / "relay-list"
    return {
        "core_config": core_config, "relays": core_config / "relays.json",
        "list_signing": core_config / "relay-list.key",
        "revocation_dir": core_config / "revocation",
        "revocation_public": core_config / "revocation" / "tracker-public.key",
        "list_dir": list_dir, "list_private": list_dir / "tracker-private.key",
        "list_public": list_dir / "tracker-public.key",
        "relay_secrets": relay_secrets, "endpoint_dir": relay_secrets / "endpoint",
        "endpoint_private": relay_secrets / "endpoint" / "tracker-private.key",
        "endpoint_public": relay_secrets / "endpoint" / "tracker-public.key",
        "tls_cert": relay_secrets / "cert.pem", "tls_key": relay_secrets / "tls.key",
    }


def relays_document(spki_sha256):
    return [{"id": RELAY_ID, "address": RELAY_EGRESS, "spki_sha256": spki_sha256,
             "control_url": RELAY_CONTROL_URL,
             "revocation_key_file": CORE_RELAY_MOUNT + "/revocation/tracker-private.key"}]


def relay_core_env(layout, tracker_public_key, *, relay_port, control_port):
    """Interpolation for compose.tracker.yaml's relay verify keys and compose.relay.yaml."""
    return {
        "ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE": str(layout["endpoint_public"]),
        "ARKNET_CORE_RELAY_CONFIG_DIR": str(layout["core_config"]),
        "ARKNET_RELAY_ID": RELAY_ID,
        "ARKNET_RELAY_GRANT_VERIFY_KEY_FILE": str(tracker_public_key),
        "ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE": str(layout["revocation_public"]),
        "ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE": str(layout["endpoint_private"]),
        "ARKNET_RELAY_TLS_CERT_FILE": str(layout["tls_cert"]),
        "ARKNET_RELAY_TLS_KEY_FILE": str(layout["tls_key"]),
        # Random loopback ports: never collide with a developer relay on 7443/7080.
        "ARKNET_RELAY_PORT": str(relay_port), "ARKNET_RELAY_CONTROL_PORT": str(control_port),
    }


def relay_node_env(list_verify_key):
    """The operator settings a Node needs for relayed peers; nothing else."""
    return {"ARKNET_RELAY_LIST_VERIFY_KEY": list_verify_key, "ARKNET_TRANSFER_RELAY_EGRESS": RELAY_EGRESS}


def core_override_document(images):
    # Core reaches the relay's control URL (revocations) over the relay's own
    # relay-edge network, never through a Node network or its database network.
    return {"services": {
        "arknet-core": {"image": images["core"], "networks": ["core", "relay-edge"]},
        "arknet-tracker": {"image": images["tracker"]},
        "arknet-relay": {"image": images["relay"]}}}


def node_override_document(images, cfg, isolated_network):
    """Both Nodes are configured identically. ``!override`` (via
    relay_acceptance.compose_override) drops compose.transfer.yaml's
    transfer-egress bridge, so the sidecar's only outside network is this
    Node's own isolated network."""
    return {"services": {
        "arknet-agent": {"image": images["agent"], "networks": ["node", "media-services", "transfer-control", "acceptance"]},
        "arknet-transfer": {"image": images["transfer"], "networks": ["transfer-control", "acceptance"]},
        "jellyfin": {"image": "jellyfin/jellyfin:10.10.7", "volumes": [
            f"{cfg['jelly_config']}:/config", f"{cfg['jelly_cache']}:/cache", "media:/srv/arknet/media:ro"]}},
        "networks": {"acceptance": {"external": True, "name": isolated_network}}}


def assert_disjoint_transfer_networks(node_a_networks, node_b_networks):
    shared = set(node_a_networks) & set(node_b_networks)
    if shared:
        raise AssertionError("direct Node-to-Node network path: transfer sidecars share " + ", ".join(sorted(shared)))


def provision_relay(run, key_helper, layout, known_secrets):
    """Dedicated disposable keys (list, revocation, endpoint) and a pinned TLS cert."""
    for directory in (layout["core_config"], layout["relay_secrets"], layout["list_dir"],
                      layout["revocation_dir"], layout["endpoint_dir"]):
        directory.mkdir(mode=0o755)
    for directory in (layout["list_dir"], layout["revocation_dir"], layout["endpoint_dir"]):
        run(["go", "run", str(key_helper), "keygen", str(directory)], timeout=120)
        private = directory / "tracker-private.key"
        os.chmod(private, 0o644)  # disposable; Core (UID 10001) / relay (UID 65534) read the bind mount
        known_secrets.append(private.read_text().strip())
    shutil.copyfile(layout["list_private"], layout["list_signing"]); os.chmod(layout["list_signing"], 0o644)
    run(["openssl", "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(layout["tls_key"])], timeout=60)
    run(["openssl", "req", "-new", "-x509", "-key", str(layout["tls_key"]), "-days", "2",
         "-subj", "/CN=arknet-relay", "-out", str(layout["tls_cert"])], timeout=60)
    os.chmod(layout["tls_key"], 0o644)
    known_secrets.append(layout["tls_key"].read_text())
    pub_pem = subprocess.run(["openssl", "x509", "-in", str(layout["tls_cert"]), "-pubkey", "-noout"],
                             capture_output=True, check=True, timeout=60).stdout
    pub_der = subprocess.run(["openssl", "pkey", "-pubin", "-outform", "DER"], input=pub_pem,
                             capture_output=True, check=True, timeout=60).stdout
    layout["relays"].write_text(json.dumps(relays_document(hashlib.sha256(pub_der).hexdigest())))
    os.chmod(layout["relays"], 0o644)
    return layout["list_public"].read_text().strip()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--results", help="write sanitized proof JSON after product evidence validation passes")
    parser.add_argument("--diagnostics-dir",
                        help="where to write the sanitized failure bundle (default: next to --results, else the temp dir)")
    args = parser.parse_args(argv)
    p25 = load_prompt25()
    p25["verify_prerequisites"]()
    run, request, raw_request_json = p25["run"], p25["request"], p25["request_json"]
    operator_headers = {}  # Agent base URL -> operator cookie header (Prompt 45B)

    def request_json(url, method="GET", body=None, headers=None, expected=(200,)):
        """Management calls to a Node's Agent carry that Node's operator session."""
        merged = dict(headers or {})
        for base, cookie in operator_headers.items():
            if url.startswith(base + "/api/v1/"):
                merged = {**cookie, **merged}
        return raw_request_json(url, method, body, merged, expected=expected)
    wait_until, jellyfin_setup = p25["wait_until"], p25["jellyfin_setup"]
    free_port = p25["free_port"]

    prefix = "arknet-p43f-test-" + uuid.uuid4().hex[:12]
    network_a = prefix + "-node-a-transport"
    network_b = prefix + "-node-b-transport"
    networks = (network_a, network_b)
    core_project, node_a_project, node_b_project = prefix + "-core", prefix + "-node-a", prefix + "-node-b"
    meter_name, helper_image = prefix + "-core-meter", prefix + "-libtorrent"
    images = {"core": prefix + "-core-image", "tracker": prefix + "-tracker-image",
              "agent": prefix + "-agent-image", "transfer": prefix + "-transfer-image", "helper": helper_image,
              "relay": prefix + "-relay-image"}
    temporary = Path(tempfile.mkdtemp(prefix="." + prefix + "-", dir=ROOT)); os.chmod(temporary, 0o755)
    failure = None; made_network = set(); known_secrets = []
    projects = (core_project, node_a_project, node_b_project)

    core_password = secrets.token_hex(32)
    known_secrets.append(core_password)
    core_port = free_port()
    core_env = dict(os.environ, POSTGRES_USER="arknet", POSTGRES_DB="arknet_core",
                    POSTGRES_PASSWORD=core_password, PGSSLMODE="disable", ARKNET_CORE_PORT=str(core_port),
                    ARKNET_TRACKER_PORT=str(free_port()), ARKNET_TRACKER_ANNOUNCE_URL="http://arknet-tracker:6969",
                    ARKNET_TRACKER_ANNOUNCE_INTERVAL="2s", ARKNET_TRACKER_PEER_TTL="30s",
                    ARKNET_CORE_WEB_SEED_ORIGIN="http://arknet-core:8088")

    node_configs = {}
    for label, project in (("a", node_a_project), ("b", node_b_project)):
        password, token, jelly_password = secrets.token_hex(32), secrets.token_hex(32), secrets.token_urlsafe(24)
        known_secrets.extend((password, token, jelly_password))
        node_configs[label] = {
            "project": project, "agent_port": free_port(), "member_port": free_port(), "jellyfin_port": free_port(),
            "password": password, "token": token, "jelly_password": jelly_password,
            "override": temporary / ("node-" + label + "-override.yaml"),
            "jelly_config": temporary / ("jellyfin-" + label + "-config"),
            "jelly_cache": temporary / ("jellyfin-" + label + "-cache"),
        }
        node_configs[label]["jelly_config"].mkdir(mode=0o755)
        node_configs[label]["jelly_cache"].mkdir(mode=0o755)
        node_configs[label]["env"] = dict(os.environ, POSTGRES_USER="arknet", POSTGRES_DB="arknet_node",
            POSTGRES_PASSWORD=password, PGSSLMODE="disable", ARKNET_AGENT_PORT=str(node_configs[label]["agent_port"]),
            ARKNET_MEMBER_PORT=str(node_configs[label]["member_port"]),
            ARKNET_UI_HTTPS_BIND="127.0.0.1:", ARKNET_TLS_DIR=str(temporary / ("tls-" + label)),
            JELLYFIN_BIND=f"127.0.0.1:{node_configs[label]['jellyfin_port']}",
            ARKNET_CORE_URL="http://arknet-core-origin:8080", ARKNET_CORE_ALLOW_HTTP="true",
            ARKNET_HEARTBEAT_INTERVAL="5s", ARKNET_MEDIA_STORAGE_ALLOCATION_GIB="1",
            ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES="", ARKNET_TRANSFER_TOKEN=token,
            ARKNET_TRANSFER_TRACKER_ORIGIN="http://arknet-tracker:6969", ARKNET_TRANSFER_TRACKER_ALLOW_HTTP="true",
            ARKNET_TRANSFER_WEB_SEED_ORIGIN="http://arknet-core:8088", ARKNET_TRANSFER_WEB_SEED_ALLOW_HTTP="true",
            ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT=str(512 * 1024), ARKNET_TRANSFER_RECONCILE_INTERVAL="5s",
            JELLYFIN_API_KEY="")

    core_override = temporary / "core-override.yaml"
    relay_files = relay_layout(temporary)
    source = temporary / "source"; source.mkdir(mode=0o755)

    def core_compose(*items, capture=False, check=True, timeout=600):
        files = [part for path in CORE_COMPOSE_FILES for part in ("-f", str(ROOT / path))]
        return run(["docker", "compose", "-p", core_project, *files, "-f", str(core_override), *items],
                   cwd=ROOT / "deploy/core", env=core_env, capture=capture, check=check, timeout=timeout)

    def node_compose(label, *items, capture=False, check=True, timeout=600):
        cfg = node_configs[label]
        return run(["docker", "compose", "-p", cfg["project"], "-f", str(ROOT / "deploy/node/compose.yaml"),
                    "-f", str(p25["REAL_TRANSFER"]), "-f", str(cfg["override"]), *items],
                   cwd=ROOT / "deploy/node", env=cfg["env"], capture=capture, check=check, timeout=timeout)

    def core_sql(statement):
        return core_compose("exec", "-T", "postgres", "psql", "-U", "arknet", "-d", "arknet_core",
                            "-At", "-v", "ON_ERROR_STOP=1", "-c", statement, capture=True)

    def transfer_status(base, object_id):
        value = request_json(base + "/api/v1/transfers")[2]
        return next((entry for entry in value["transfers"] if entry["object_id"] == object_id), None)

    def transfer_container(label):
        return node_compose(label, "ps", "-q", "arknet-transfer", capture=True)

    def transfer_seeds(label):
        script = ("import json,os,urllib.request; r=urllib.request.Request("
                  "'http://127.0.0.1:8084/api/transfer/v1/seeds',headers={"
                  "'Authorization':'Bearer '+os.environ['ARKNET_TRANSFER_TOKEN']});"
                  "print(urllib.request.urlopen(r,timeout=5).read().decode())")
        output = run(["docker", "exec", transfer_container(label), "python3", "-c", script],
                     capture=True, timeout=20)
        return json.loads(output)["seeds"]

    def transfer_tx_bytes(label):
        script = ("import pathlib; print(sum(int(p.read_text()) for p in "
                  "pathlib.Path('/sys/class/net').glob('*/statistics/tx_bytes') "
                  "if '/lo/' not in str(p)))")
        return int(run(["docker", "exec", transfer_container(label), "python3", "-c", script],
                       capture=True, timeout=20))

    def transfer_networks(label):
        output = run(["docker", "inspect", "-f", "{{json .NetworkSettings.Networks}}", transfer_container(label)],
                     capture=True, timeout=20)
        return set(json.loads(output))

    def relay_tx_bytes():
        # Summed non-loopback tx counters of the relay container (busybox sh).
        script = 'for d in /sys/class/net/*; do [ "${d##*/}" = lo ] || cat "$d/statistics/tx_bytes"; done'
        relay_id = core_compose("ps", "-q", "arknet-relay", capture=True)
        output = run(["docker", "exec", relay_id, "sh", "-c", script], capture=True, timeout=20)
        return sum(int(line) for line in output.split())

    def meter(method="GET"):
        command = ["docker", "exec", meter_name, "python3", "-c",
                   "import urllib.request; r=urllib.request.Request('http://127.0.0.1:8088/" +
                   ("__reset',method='POST'); print(urllib.request.urlopen(r).status)" if method == "POST" else
                    "__stats'); print(urllib.request.urlopen(r).read().decode())")]
        output = run(command, capture=True, timeout=20)
        return None if method == "POST" else json.loads(output)

    def node_state(base, wanted):
        status, _, value = request_json(base + "/api/v1/status")
        return value if status == 200 and value.get("state") == wanted else None

    try:
        key_helper = temporary / "grant-helper.go"; key_helper.write_text(p25["KEY_HELPER"]); os.chmod(key_helper, 0o600)
        make_torrent = temporary / "make-torrent.py"; make_torrent.write_text(p25["MAKE_TORRENT"]); os.chmod(make_torrent, 0o600)
        meter_script = temporary / "meter.py"; meter_script.write_text(METER_PROXY); os.chmod(meter_script, 0o600)
        run(["go", "run", str(key_helper), "keygen", str(temporary)], timeout=120)
        private_key, public_key = temporary / "tracker-private.key", temporary / "tracker-public.key"
        os.chmod(private_key, 0o644)
        known_secrets.append(private_key.read_text().strip())
        core_env.update(ARKNET_TRACKER_SIGNING_KEY_FILE=str(private_key), ARKNET_TRACKER_VERIFY_KEY_FILE=str(public_key),
                        ARKNET_CORE_WEB_SEED_SOURCE=str(source))
        # Private relay is the product's only peer transport (Prompt 48D).
        list_verify_key = provision_relay(run, key_helper, relay_files, known_secrets)
        core_env.update(relay_core_env(relay_files, public_key, relay_port=free_port(), control_port=free_port()))
        for cfg in node_configs.values():
            cfg["env"].update(relay_node_env(list_verify_key))

        object_id = "media_" + secrets.token_hex(32); video = source / object_id
        run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
             "-i", "testsrc2=size=1280x720:rate=30", "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000",
             "-t", "40", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "5M", "-maxrate", "5M",
             "-bufsize", "10M", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart",
             "-f", "mp4", str(video)], timeout=300)
        object_size = video.stat().st_size
        if object_size < p25["MIN_VIDEO_BYTES"]: raise AssertionError("generated playable object is too small")
        probe = json.loads(run(["ffprobe", "-v", "error", "-show_entries", "format=format_name,duration", "-show_streams",
                                "-of", "json", str(video)], capture=True, timeout=30))
        if "mp4" not in probe["format"]["format_name"] or {s["codec_type"] for s in probe["streams"]} < {"video", "audio"}:
            raise AssertionError("generated object is not a playable audio/video MP4")
        digest = hashlib.sha256(video.read_bytes()).hexdigest()

        run(["docker", "build", "-t", helper_image, "-"], input_text=p25["LIBTORRENT_DOCKERFILE"], timeout=600)
        torrent = json.loads(run(["docker", "run", "--rm", "-v", f"{temporary}:/work",
                                  "-v", f"{make_torrent}:/make-torrent.py:ro", helper_image,
                                  "/make-torrent.py", "/work", object_id, str(object_size)], capture=True, timeout=180))
        if torrent["size"] != object_size or torrent["pieces"] <= 1 or len(torrent["info_hash_v2"]) != 64:
            raise AssertionError("torrent is not private v2-only multi-piece metadata")
        info_hash = torrent["info_hash_v2"]
        shutil.copyfile(temporary / "object.torrent", source / (object_id + ".torrent"))

        for isolated_network in networks:
            run(["docker", "network", "create", isolated_network], capture=True)
            made_network.add(isolated_network)
        core_override.write_text(relay_acceptance.compose_override(core_override_document(images)))
        for label in ("a", "b"):
            cfg = node_configs[label]
            isolated_network = network_a if label == "a" else network_b
            cfg["override"].write_text(relay_acceptance.compose_override(node_override_document(images, cfg, isolated_network)))
            os.chmod(cfg["override"], 0o600)
        os.chmod(core_override, 0o600)

        core_compose("up", "--build", "-d", "--wait", "--wait-timeout", "180")
        core_id = core_compose("ps", "-q", "arknet-core", capture=True)
        tracker_id = core_compose("ps", "-q", "arknet-tracker", capture=True)
        relay_id = core_compose("ps", "-q", "arknet-relay", capture=True)
        # Gateway services only: each Node network sees Core, tracker, meter and
        # relay, never the other Node. Attached before the Nodes start because the
        # transfer sidecar resolves its egress allowlist once at boot.
        for isolated_network in networks:
            run(["docker", "network", "connect", "--alias", "arknet-core-origin", isolated_network, core_id])
            run(["docker", "network", "connect", "--alias", "arknet-tracker", isolated_network, tracker_id])
            run(["docker", "network", "connect", "--alias", "arknet-relay", isolated_network, relay_id])
        run(["docker", "run", "-d", "--name", meter_name, "--network", network_a,
             "--network-alias", "arknet-core",
             "-e", "UPSTREAM=http://arknet-core-origin:8080", "-e", "RATE_BYTES_PER_SECOND=" + str(96 * 1024),
             "-v", f"{meter_script}:/meter.py:ro", helper_image, "/meter.py"], capture=True)
        run(["docker", "network", "connect", "--alias", "arknet-core", network_b, meter_name])
        wait_until("meter proxy", lambda: "ready" in run(["docker", "logs", meter_name], capture=True, check=False), 30)

        for label in ("a", "b"):
            build = ["--build"] if label == "a" else []
            node_compose(label, "up", *build, "-d", "--wait", "--wait-timeout", "240", "postgres", "jellyfin", "arknet-transfer")
            cfg = node_configs[label]; jelly_base = f"http://127.0.0.1:{cfg['jellyfin_port']}"
            wait_until(label + " Jellyfin HTTP", lambda base=jelly_base: request(base + "/System/Info/Public")[0] == 200, 180)
            access, key = jellyfin_setup(jelly_base, cfg["jelly_password"]); known_secrets.extend((access, key))
            cfg["env"]["JELLYFIN_API_KEY"] = key
            node_compose(label, "up", "-d", "--wait", "--wait-timeout", "180", "arknet-agent")
            cfg["base"] = f"http://127.0.0.1:{cfg['agent_port']}"
            cfg["member_base"] = f"http://127.0.0.1:{cfg['member_port']}"
            code = operator_login.setup_code(node_compose(label, "exec", "-T", "arknet-agent", "arknet-agent", "operator",
                                                          "setup-code", capture=True))
            known_secrets.extend((code, code.replace("-", "")))
            signed_in = operator_login.bootstrap(cfg["base"], code, request, username=f"operator-{label}",
                                                 member_base=cfg["member_base"])
            known_secrets.extend(signed_in.secrets)
            operator_headers[cfg["base"]] = signed_in.headers

        # Peer bytes can only move through the relay: the sidecars share no network.
        assert_disjoint_transfer_networks(transfer_networks("a"), transfer_networks("b"))
        print("SETUP: Core, tracker, relay, meter, Node A and Node B running; no Node-to-Node network path", flush=True)

        core_admin = ScopedAdmin(core_compose, "multi-node-swarm", "nodes.decide")
        admin_headers = core_admin.headers()
        known_secrets.extend(core_admin.secrets())
        for label in ("a", "b"):
            cfg = node_configs[label]
            node = wait_until(label + " enrollment", lambda c=cfg: node_state(c["base"], "ENROLLMENT_PENDING"), 120)
            cfg["node_id"] = node["node_id"]
            request_json(f"http://127.0.0.1:{core_port}/api/admin/v1/nodes/{node['node_id']}/approve", "POST", headers=admin_headers)
            wait_until(label + " connected", lambda c=cfg: node_state(c["base"], "CONNECTED"), 120)

        core_sql("INSERT INTO media_objects (public_id,content_type,title,size,state,info_hash_v2) VALUES "
                 f"('{object_id}','VIDEO','Prompt 43F playable acceptance',{object_size},'ACTIVE','{info_hash}')")

        # Node A's real operator path is the measured Core-only baseline.
        meter("POST"); base_a = node_configs["a"]["base"]
        request_json(base_a + f"/api/v1/media/{object_id}/selection", "POST", {})
        started_a = request_json(base_a + f"/api/v1/media/{object_id}/transfer/start", "POST", {})[2]
        if started_a["mock"] is not False: raise AssertionError("Node A did not start a real transfer")
        wait_until("Node A exact AVAILABLE replica", lambda: next((o for o in request_json(base_a + "/api/v1/media")[2]["objects"]
                   if o["id"] == object_id and o["local_state"] == "AVAILABLE"), None), 600)
        core_only_egress = meter()["payload_bytes"]
        if core_only_egress <= 0: raise AssertionError("Core-only baseline measured no web-seed payload")

        # Prove the negative path against an active Node B download. Node A is
        # AVAILABLE but Peer Serving is still OFF, so the reconciler must not
        # install a seed and its transfer container must send control traffic only.
        node_a_id = node_configs["a"]["node_id"]
        eligible_sql = ("SELECT n.public_id FROM media_replicas r JOIN nodes n ON n.id=r.node_id "
                        "JOIN media_objects m ON m.id=r.media_object_id WHERE m.public_id='" + object_id + "' "
                        "AND m.state='ACTIVE' AND n.enrollment_status='APPROVED' AND n.trust_status='TRUSTED' "
                        "AND n.peer_serving=true ORDER BY n.public_id LIMIT 10")
        wait_until("Node A has no seed while Peer Serving OFF", lambda: transfer_seeds("a") == [], 30)
        if core_sql(eligible_sql) != "":
            raise AssertionError("Node A was source-eligible while Peer Serving OFF")

        meter("POST"); base_b = node_configs["b"]["base"]
        started_at = time.monotonic()
        tx_off_start = transfer_tx_bytes("a")
        request_json(base_b + f"/api/v1/media/{object_id}/selection", "POST", {})
        # The operator's explicit start is the product's only path from QUEUED to a
        # transfer; the reconciler never starts queued media on its own.
        started_b = request_json(base_b + f"/api/v1/media/{object_id}/transfer/start", "POST", {})[2]
        if started_b["mock"] is not False:
            raise AssertionError("Node B did not start a real transfer")
        wait_until("Node B progresses from Core while Node A is disabled", lambda: (
            (status := transfer_status(base_b, object_id)) and status["completed_bytes"] >= 1024 * 1024), 90)
        node_a_tx_while_off = transfer_tx_bytes("a") - tx_off_start
        control_traffic_limit = 128 * 1024
        if node_a_tx_while_off > control_traffic_limit:
            raise AssertionError("Node A sent more than control traffic while Peer Serving OFF")
        if transfer_seeds("a"):
            raise AssertionError("Node A installed a seed while Peer Serving OFF")

        # Enable the real product path and wait through heartbeat, Core source
        # eligibility, the Agent reconciler, transfer API, and worker hash check.
        request_json(base_a + "/api/v1/settings/peer-serving", "PUT", {"enabled": True},
                     {"Content-Type": "application/json"})
        wait_until("Node A heartbeat peer_serving=true", lambda: core_sql(
            f"SELECT peer_serving FROM nodes WHERE public_id='{node_a_id}'") == "t", 60)
        wait_until("Node A Core source eligibility", lambda: core_sql(eligible_sql) == node_a_id, 60)
        wait_until("Node A product seed reaches SEEDING", lambda: any(
            seed["object_id"] == object_id and seed["state"] == "SEEDING" for seed in transfer_seeds("a")), 120)

        seed_tx_start = transfer_tx_bytes("a")
        relay_tx_start = relay_tx_bytes()
        stopped_progress = stopped_core = node_a_upload = relay_forwarded = None
        deadline = time.monotonic() + 300
        while time.monotonic() < deadline:
            status = transfer_status(base_b, object_id)
            node_a_upload = transfer_tx_bytes("a") - seed_tx_start
            if node_a_upload >= 2 * 1024 * 1024 and status and 0 < status["completed_bytes"] < object_size:
                stopped_progress = status["completed_bytes"]
                stopped_core = meter()["payload_bytes"]
                request_json(base_a + "/api/v1/settings/peer-serving", "PUT", {"enabled": False},
                             {"Content-Type": "application/json"})
                wait_until("Peer Serving OFF unseeds Node A", lambda: transfer_seeds("a") == [], 30)
                node_a_upload = transfer_tx_bytes("a") - seed_tx_start
                relay_forwarded = relay_tx_bytes() - relay_tx_start
                break
            if status and status["completed_bytes"] >= object_size:
                raise AssertionError("Node B completed before Peer Serving could be disabled")
            time.sleep(.1)
        if stopped_progress is None: raise AssertionError("Node A supplied no measurable payload before timeout")
        # Node A's only reachable peer destination is the relay, so its upload
        # must reappear as relay egress toward Node B.
        if relay_forwarded < 1024 * 1024:
            raise AssertionError("relay forwarded less than 1 MiB while Node A seeded; peer payload did not cross the relay")

        # Let any already-written socket bytes drain, then prove traffic remains
        # bounded while Node B continues from Core with the seed removed.
        time.sleep(2)
        tx_disabled_start = transfer_tx_bytes("a")
        disabled_progress_target = min(object_size, stopped_progress + 1024 * 1024)
        wait_until("Node B progresses after Node A unseed", lambda: (
            (status := transfer_status(base_b, object_id)) and status["completed_bytes"] >= disabled_progress_target), 90)
        node_a_tx_after_disable = transfer_tx_bytes("a") - tx_disabled_start
        if node_a_tx_after_disable > control_traffic_limit:
            raise AssertionError("Node A continued sending payload after Peer Serving was disabled")

        wait_until("Node B exact AVAILABLE replica", lambda: next((o for o in request_json(base_b + "/api/v1/media")[2]["objects"]
                   if o["id"] == object_id and o["local_state"] == "AVAILABLE"), None), 600)
        elapsed = time.monotonic() - started_at
        final_status = transfer_status(base_b, object_id)
        core_egress = meter()["payload_bytes"]
        media_volume_b = node_b_project + "_media"
        b_hash = run(["docker", "run", "--rm", "-v", f"{media_volume_b}:/media:ro", helper_image, "-c",
                      "import hashlib,pathlib; print(hashlib.sha256(pathlib.Path('/media/" + object_id + ".mp4').read_bytes()).hexdigest())"],
                     capture=True, timeout=120)
        if b_hash != digest: raise AssertionError("Node B completed bytes do not match the source SHA-256")

        evidence_values = {"object_size": object_size, "core_only_egress": core_only_egress,
            "core_egress": core_egress, "node_a_upload": node_a_upload,
            "node_b_completed": final_status["completed_bytes"], "node_b_elapsed_seconds": elapsed,
            "node_b_bytes_when_a_stopped": stopped_progress, "core_egress_when_a_stopped": stopped_core,
            "node_a_tx_while_peer_serving_off": node_a_tx_while_off,
            "node_a_tx_after_peer_serving_disabled": node_a_tx_after_disable,
            "control_traffic_limit": control_traffic_limit, "relay_forwarded_bytes": relay_forwarded}
        # Measurements are plain byte counts and timings; safe to print on failure.
        print("Measurements: " + json.dumps(evidence_values, sort_keys=True), flush=True)
        result = result_document(run_id=prefix, object_id=object_id, object_sha256=digest,
                                 evidence_values=evidence_values)
        if args.results: write_result(args.results, result)
        print("PASS: Prompt 43F product-path evidence validated\n" + json.dumps(result, indent=2, sort_keys=True), flush=True)
    except Exception as error:
        failure = error
        # Collect before cleanup, while every container still exists.
        def logs(compose, *services, tail="200"):
            return compose("logs", "--no-color", "--timestamps", "--tail", tail, *services,
                           capture=True, check=False, timeout=60)

        def node_api(label, path):
            return request_json(node_configs[label]["base"] + path)[2]

        collectors = {
            "failure": lambda: e2e_diagnostics.failure_text(error),
            "core-logs": lambda: logs(core_compose, "arknet-core", "arknet-tracker", "arknet-relay"),
            "meter-log": lambda: run(["docker", "logs", "--tail", "400", meter_name], capture=True, check=False, timeout=30),
            "meter-stats": lambda: meter(),
        }
        for label in ("a", "b"):
            compose = (lambda l: lambda *i, **k: node_compose(l, *i, **k))(label)
            collectors.update({
                f"node-{label}-status": lambda l=label: node_api(l, "/api/v1/status"),
                f"node-{label}-transfers": lambda l=label: node_api(l, "/api/v1/transfers"),
                f"node-{label}-media": lambda l=label: node_api(l, "/api/v1/media"),
                f"node-{label}-seeds": lambda l=label: transfer_seeds(l),
                f"node-{label}-worker-events": lambda c=compose: "\n".join(
                    line for line in logs(c, "arknet-transfer", tail="2000").splitlines() if '"event"' in line)
                    or "<no worker events>",
                f"node-{label}-logs": lambda c=compose: logs(c, "arknet-agent", "arknet-transfer"),
            })
        directory = Path(args.diagnostics_dir) if args.diagnostics_dir else e2e_diagnostics.default_directory(prefix, args.results)
        try:
            sections = e2e_diagnostics.collect(collectors)
            bundle = e2e_diagnostics.write_bundle(directory, sections, known_secrets, temporary)
            print(f"DIAGNOSTICS: wrote {len(sections)} sanitized sections to {bundle} "
                  f"({e2e_diagnostics.scan(bundle, known_secrets)} known secret values present)", flush=True)
        except Exception as bundle_error:
            print(f"ERROR: diagnostics bundle not written: {type(bundle_error).__name__}", flush=True)
        raise
    finally:
        run(["docker", "rm", "-f", meter_name], check=False, timeout=30)
        for label in ("b", "a"): node_compose(label, "down", "--volumes", "--remove-orphans", check=False, timeout=180)
        core_compose("down", "--volumes", "--remove-orphans", check=False, timeout=180)
        for isolated_network in networks:
            if isolated_network in made_network:
                run(["docker", "network", "rm", isolated_network], check=False, timeout=30)
        for image in images.values(): run(["docker", "image", "rm", "-f", image], check=False, timeout=60)
        harness_scratch.remove_tree(temporary)  # Linux containers leave root-owned files
        leftovers = []
        for project in projects:
            for kind, command in (("container", ["docker", "ps", "-aq", "--filter", f"label=com.docker.compose.project={project}"]),
                                  ("volume", ["docker", "volume", "ls", "-q", "--filter", f"label=com.docker.compose.project={project}"]),
                                  ("network", ["docker", "network", "ls", "-q", "--filter", f"label=com.docker.compose.project={project}"])):
                output = run(command, capture=True, check=False, timeout=30)
                if output: leftovers.append(f"{kind}:{project}:{output.replace(chr(10), ',')}")
        named_resources = (("container", meter_name),
                           ("network", network_a), ("network", network_b))
        for kind, name in named_resources:
            if subprocess.run(["docker", kind, "inspect", name], cwd=ROOT, stdout=subprocess.DEVNULL,
                              stderr=subprocess.DEVNULL, timeout=20, check=False).returncode == 0: leftovers.append(kind + ":" + name)
        for image in images.values():
            if subprocess.run(["docker", "image", "inspect", image], cwd=ROOT, stdout=subprocess.DEVNULL,
                              stderr=subprocess.DEVNULL, timeout=20, check=False).returncode == 0: leftovers.append("image:" + image)
        if temporary.exists(): leftovers.append("directory:" + str(temporary))
        if leftovers:
            cleanup_error = RuntimeError("exact cleanup verification failed: " + ", ".join(leftovers))
            if failure is None: raise cleanup_error
            print("ERROR: " + str(cleanup_error), flush=True)
        else: print("PASS: exact scoped cleanup removed this run's resources and secrets", flush=True)


if __name__ == "__main__":
    main()
