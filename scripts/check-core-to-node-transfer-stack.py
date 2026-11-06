#!/usr/bin/env python3
"""Disposable Prompt 25 Core-to-Node real-transfer acceptance check.

Every Docker resource and temporary secret uses one random prefix. Cleanup names
exact resources and verifies readback; this script never prunes Docker or reads a
developer .env file.
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from scripts.core_admin import ScopedAdmin  # noqa: E402
REAL_TRANSFER = ROOT / "deploy/node/compose.transfer.yaml"
MIN_VIDEO_BYTES = 16 * 1024 * 1024

KEY_HELPER = r'''
package main
import (
 "crypto/ed25519"
 "crypto/rand"
 "crypto/sha256"
 "encoding/base64"
 "encoding/binary"
 "encoding/hex"
 "fmt"
 "os"
 "path/filepath"
 "time"
)
func main() {
 if len(os.Args) < 3 { panic("usage: helper keygen DIR | issue KEY HASH") }
 if os.Args[1] == "keygen" {
  pub, priv, err := ed25519.GenerateKey(rand.Reader); if err != nil { panic(err) }
  dir := os.Args[2]
  if err = os.WriteFile(filepath.Join(dir,"tracker-private.key"), []byte(base64.StdEncoding.EncodeToString(priv)), 0600); err != nil { panic(err) }
  if err = os.WriteFile(filepath.Join(dir,"tracker-public.key"), []byte(base64.StdEncoding.EncodeToString(pub)), 0644); err != nil { panic(err) }
  return
 }
 if os.Args[1] != "issue" || len(os.Args) != 4 { panic("bad operation") }
 encoded, err := os.ReadFile(os.Args[2]); if err != nil { panic(err) }
 privateBytes, err := base64.StdEncoding.DecodeString(string(encoded)); if err != nil || len(privateBytes) != ed25519.PrivateKeySize { panic("bad key") }
 hash, err := hex.DecodeString(os.Args[3]); if err != nil || len(hash) != 32 { panic("bad hash") }
 key := ed25519.PrivateKey(privateBytes)
 payload := make([]byte, 58); payload[0] = 2
 binary.BigEndian.PutUint64(payload[1:9], uint64(time.Now().Add(30*time.Minute).Unix()))
 subjectMessage := append([]byte("arknet-tracker-subject-v1\x00acceptance-source\x00"), hash...)
 subject := sha256.Sum256(ed25519.Sign(key, subjectMessage)); copy(payload[9:25], subject[:16])
 copy(payload[25:57], hash); payload[57] = 2 // seed only
 signature := ed25519.Sign(key, payload)
 fmt.Print(base64.RawURLEncoding.EncodeToString(payload)+"."+base64.RawURLEncoding.EncodeToString(signature))
}
'''

MAKE_TORRENT = r'''
import json
from pathlib import Path
import sys
import libtorrent as lt
root, object_id = Path(sys.argv[1]), sys.argv[2]
source = root / "source" / object_id
files = lt.file_storage(); files.add_file(object_id, source.stat().st_size)
creator = lt.create_torrent(files, 256 * 1024, lt.create_torrent.v2_only)
creator.set_priv(True); creator.add_tracker("http://arknet-tracker:6969/announce")
lt.set_piece_hashes(creator, str(source.parent))
encoded = lt.bencode(creator.generate())
(root / "object.torrent").write_bytes(encoded)
info = lt.torrent_info(encoded); hashes = info.info_hashes()
assert info.priv() and hashes.has_v2() and not hashes.has_v1()
print(json.dumps({"info_hash_v2": str(hashes.v2).lower(), "size": source.stat().st_size,
                  "pieces": info.num_pieces()}))
'''

SEEDER = r'''
import json
from pathlib import Path
import sys
import time
import libtorrent as lt
root, object_id = Path(sys.argv[1]), sys.argv[2]
secret = json.loads((root / "seed-secret.json").read_text())
settings = {
 "enable_dht": False, "enable_lsd": False, "enable_upnp": False,
 "enable_natpmp": False, "broadcast_lsd": False, "max_pex_peers": 0,
 "listen_interfaces": "0.0.0.0:6881", "upload_rate_limit": 1024,
 "announce_to_all_trackers": False, "announce_to_all_tiers": False,
 "prefer_udp_trackers": False, "use_dht_as_fallback": False,
}
session = lt.session(settings); session.add_extension(lt.create_ut_metadata_plugin)
params = lt.add_torrent_params(); params.ti = lt.torrent_info(str(root / "object.torrent"))
params.save_path = str(root / "source"); params.trackers = [secret["tracker_url"]]
params.flags |= (lt.torrent_flags.disable_dht |
                 lt.torrent_flags.disable_lsd | lt.torrent_flags.disable_pex |
                 lt.torrent_flags.override_trackers | lt.torrent_flags.override_web_seeds)
handle = session.add_torrent(params)
while not handle.status().is_seeding:
 status = handle.status()
 if status.errc.value() != 0: raise RuntimeError("seed validation failed")
 time.sleep(.1)
print(json.dumps({"ready": True, "private_v2_seed": True}), flush=True)
while True:
 status = handle.status()
 if status.errc.value() != 0: raise RuntimeError("seed failed")
 time.sleep(.5)
'''

LIBTORRENT_DOCKERFILE = """FROM ubuntu:24.04
RUN apt-get update -qq \\
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends python3 python3-libtorrent ca-certificates \\
 && rm -rf /var/lib/apt/lists/*
ENTRYPOINT [\"python3\"]
"""


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def run(command, *, cwd=ROOT, env=None, capture=False, input_text=None,
        timeout=600, check=True):
    result = subprocess.run(command, cwd=cwd, env=env, check=check, text=True,
                            input=input_text, capture_output=capture, timeout=timeout)
    return result.stdout.strip() if capture else ""


def request(url, method="GET", body=None, headers=None, timeout=20):
    data = body
    request_headers = dict(headers or {})
    if body is not None and not isinstance(body, bytes):
        data = json.dumps(body, separators=(",", ":")).encode()
        request_headers.setdefault("Content-Type", "application/json")
    req = urllib.request.Request(url, data=data, method=method, headers=request_headers)
    try:
        response = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, dict(response.headers), response.read()


def request_json(url, method="GET", body=None, headers=None, expected=(200,)):
    status, response_headers, data = request(url, method, body, headers)
    if status not in expected:
        raise AssertionError(f"{method} {urllib.parse.urlsplit(url).path}: HTTP {status}: {data[:300]!r}")
    value = json.loads(data) if data else None
    return status, response_headers, value


def wait_until(description, probe, seconds=120, interval=.5):
    deadline = time.monotonic() + seconds
    last = None
    while time.monotonic() < deadline:
        try:
            value = probe()
            if value:
                return value
            last = value
        except Exception as error:  # startup and reconciliation are eventually consistent
            last = error
        time.sleep(interval)
    raise AssertionError(f"timed out waiting for {description}; last={last!r}")


def jellyfin_setup(base, password):
    username, login_password = "arknet-acceptance", password
    status, _, startup = request(base + "/Startup/Configuration")
    if status == 200:
        request_json(base + "/Startup/Configuration", "POST", {
            "UICulture": "en-US", "MetadataCountryCode": "US",
            "PreferredMetadataLanguage": "en", "ServerName": "ArkNet acceptance",
        }, expected=(204,))
        user_body = json.dumps({"Name": username, "Password": password}, separators=(",", ":")).encode()
        user_status, _, _ = request(base + "/Startup/User", "POST", user_body,
                                    {"Content-Type": "application/json"})
        if user_status == 500:
            # Some images initialise a named placeholder, making Startup/User
            # unable to find the blank-name row. Read that local-only setup
            # identity and authenticate it without persisting any credentials.
            get_status, _, setup_user = request(base + "/Startup/User")
            if get_status != 200:
                raise AssertionError(f"GET /Startup/User: HTTP {get_status}")
            setup_value = json.loads(setup_user)
            username, login_password = setup_value.get("Name", "jellyfin"), ""
        elif user_status != 204:
            raise AssertionError(f"POST /Startup/User: HTTP {user_status}")
        request_json(base + "/Startup/RemoteAccess", "POST", {
            "EnableRemoteAccess": False, "EnableAutomaticPortMapping": False,
        }, expected=(204,))
        request_json(base + "/Startup/Complete", "POST", {}, expected=(204,))
    authorization = ('MediaBrowser Client="ArkNet acceptance", Device="acceptance", '
                     'DeviceId="arknet-prompt-25", Version="0.0.1-dev"')
    _, _, login = request_json(base + "/Users/AuthenticateByName", "POST", {
        "Username": username, "Pw": login_password,
    }, headers={"Authorization": authorization})
    access_token = login["AccessToken"]
    auth = {"X-Emby-Token": access_token}
    # Create an empty Movies library, then add the read-only ArkNet media path.
    status, _, _ = request(base + "/Library/VirtualFolders?" + urllib.parse.urlencode({
        "name": "ArkNet", "collectionType": "movies", "refreshLibrary": "false",
    }), "POST", b"", auth)
    if status not in (204, 409):
        raise AssertionError(f"Jellyfin library creation returned {status}")
    status, _, _ = request_json(base + "/Library/VirtualFolders/Paths?" + urllib.parse.urlencode({
        "name": "ArkNet", "refreshLibrary": "true",
    }), "POST", {"Name": "ArkNet", "Path": "/srv/arknet/media"}, auth, expected=(204, 409))
    request_json(base + "/Auth/Keys?app=arknet-prompt-25", "POST", {}, auth, expected=(204,))
    _, _, keys = request_json(base + "/Auth/Keys", headers=auth)
    api_key = next(key["AccessToken"] for key in keys["Items"] if key["AppName"] == "arknet-prompt-25")
    return access_token, api_key


def verify_prerequisites():
    missing = []
    if not REAL_TRANSFER.is_file():
        missing.append(str(REAL_TRANSFER.relative_to(ROOT)))
    checks = {
        ROOT / "node-agent/internal/catalog/client.go": ("ReportReplica", "ExpiresAt", "Actions", "TorrentURL"),
        ROOT / "node-agent/internal/transfer/client.go": ("TrackerURL", "WebSeedURL", "TorrentURL"),
        ROOT / "node-agent/transfer_finalizer.go": ("Available", "reportReplica"),
        ROOT / "transfer/transfers.go": ("tracker_url", "web_seed_url"),
    }
    for path, needles in checks.items():
        text = path.read_text() if path.exists() else ""
        absent = [needle for needle in needles if needle not in text]
        if absent:
            missing.append(f"{path.relative_to(ROOT)} ({', '.join(absent)})")
    if missing:
        raise RuntimeError("Prompt 25 runtime contracts are not available: " + "; ".join(missing))


def main():
    verify_prerequisites()
    prefix = "arknet-p25-test-" + uuid.uuid4().hex[:12]
    network = prefix + "-transport"
    core_project, node_project = prefix + "-core", prefix + "-node"
    seeder_name, helper_image = prefix + "-source", prefix + "-libtorrent"
    images = {
        "core": prefix + "-core-image", "tracker": prefix + "-tracker-image",
        "agent": prefix + "-agent-image", "transfer": prefix + "-transfer-image",
        "helper": helper_image,
    }
    temporary = Path(tempfile.mkdtemp(prefix="." + prefix + "-", dir=ROOT))
    os.chmod(temporary, 0o755)
    failure = None
    known_secrets = []
    made_network = False
    core_port, agent_port, jellyfin_port = free_port(), free_port(), free_port()
    transfer_token = secrets.token_hex(32)
    core_password, node_password = secrets.token_hex(32), secrets.token_hex(32)
    jellyfin_password = secrets.token_urlsafe(24)
    known_secrets.extend([transfer_token, core_password, node_password, jellyfin_password])

    core_env = dict(os.environ, POSTGRES_USER="arknet", POSTGRES_DB="arknet_core",
                    POSTGRES_PASSWORD=core_password, PGSSLMODE="disable",
                    ARKNET_CORE_PORT=str(core_port),
                    ARKNET_TRACKER_PORT=str(free_port()),
                    ARKNET_TRACKER_ANNOUNCE_URL="http://arknet-tracker:6969",
                    ARKNET_TRACKER_ANNOUNCE_INTERVAL="5s", ARKNET_TRACKER_PEER_TTL="30s",
                    ARKNET_CORE_WEB_SEED_ORIGIN="http://arknet-core:8080")
    node_env = dict(os.environ, POSTGRES_USER="arknet", POSTGRES_DB="arknet_node",
                    POSTGRES_PASSWORD=node_password, PGSSLMODE="disable",
                    ARKNET_AGENT_PORT=str(agent_port), JELLYFIN_BIND=f"127.0.0.1:{jellyfin_port}",
                    ARKNET_CORE_URL="http://arknet-core:8080", ARKNET_CORE_ALLOW_HTTP="true",
                    ARKNET_HEARTBEAT_INTERVAL="5s", ARKNET_MEDIA_STORAGE_ALLOCATION_GIB="1",
                    ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES="", ARKNET_TRANSFER_TOKEN=transfer_token,
                    ARKNET_TRANSFER_TRACKER_ORIGIN="http://arknet-tracker:6969",
                    ARKNET_TRANSFER_TRACKER_ALLOW_HTTP="true",
                    ARKNET_TRANSFER_WEB_SEED_ORIGIN="http://arknet-core:8080",
                    ARKNET_TRANSFER_WEB_SEED_ALLOW_HTTP="true",
                    ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT=str(512 * 1024),
                    JELLYFIN_API_KEY="")

    core_override = temporary / "core-override.json"
    node_override = temporary / "node-override.json"

    def core_compose(*args, capture=False, check=True, timeout=600):
        return run(["docker", "compose", "-p", core_project,
                    "-f", str(ROOT / "deploy/core/compose.yaml"),
                    "-f", str(ROOT / "deploy/core/compose.tracker.yaml"),
                    "-f", str(ROOT / "deploy/core/compose.web-seed.yaml"),
                    "-f", str(core_override), *args], cwd=ROOT / "deploy/core",
                   env=core_env, capture=capture, check=check, timeout=timeout)

    def node_compose(*args, capture=False, check=True, timeout=600):
        return run(["docker", "compose", "-p", node_project,
                    "-f", str(ROOT / "deploy/node/compose.yaml"),
                    "-f", str(REAL_TRANSFER), "-f", str(node_override), *args],
                   cwd=ROOT / "deploy/node", env=node_env, capture=capture,
                   check=check, timeout=timeout)

    def core_sql(statement):
        return core_compose("exec", "-T", "postgres", "psql", "-U", "arknet",
                            "-d", "arknet_core", "-At", "-v", "ON_ERROR_STOP=1",
                            "-c", statement, capture=True)

    def transfer_status(agent_base, object_id):
        _, _, result = request_json(agent_base + "/api/v1/transfers")
        assert result["enabled"] and result["available"]
        return next((entry for entry in result["transfers"] if entry["object_id"] == object_id), None)

    try:
        source = temporary / "source"; source.mkdir(mode=0o755)
        jellyfin_config = temporary / "jellyfin-config"; jellyfin_config.mkdir(mode=0o755)
        jellyfin_cache = temporary / "jellyfin-cache"; jellyfin_cache.mkdir(mode=0o755)
        key_helper = temporary / "grant-helper.go"; key_helper.write_text(KEY_HELPER); os.chmod(key_helper, 0o600)
        make_torrent = temporary / "make-torrent.py"; make_torrent.write_text(MAKE_TORRENT); os.chmod(make_torrent, 0o600)
        seeder_script = temporary / "seeder.py"; seeder_script.write_text(SEEDER); os.chmod(seeder_script, 0o600)
        run(["go", "run", str(key_helper), "keygen", str(temporary)], timeout=120)
        private_key, public_key = temporary / "tracker-private.key", temporary / "tracker-public.key"
        os.chmod(private_key, 0o644)  # Core's non-root UID must read the disposable bind mount.
        core_env.update(ARKNET_TRACKER_SIGNING_KEY_FILE=str(private_key),
                        ARKNET_TRACKER_VERIFY_KEY_FILE=str(public_key),
                        ARKNET_CORE_WEB_SEED_SOURCE=str(source))

        object_id = "media_" + secrets.token_hex(32)
        video = source / object_id
        run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
             "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30",
             "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000",
             "-t", "35", "-c:v", "libx264", "-preset", "ultrafast",
             "-b:v", "5M", "-maxrate", "5M", "-bufsize", "10M",
             "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k",
             "-movflags", "+faststart", "-f", "mp4", str(video)], timeout=300)
        video_size = video.stat().st_size
        if video_size < MIN_VIDEO_BYTES:
            raise AssertionError(f"generated MP4 is only {video_size} bytes")
        probe = json.loads(run(["ffprobe", "-v", "error", "-show_entries",
                                "format=format_name,duration", "-show_streams",
                                "-of", "json", str(video)], capture=True, timeout=30))
        assert "mp4" in probe["format"]["format_name"] and float(probe["format"]["duration"]) >= 34
        assert {stream["codec_type"] for stream in probe["streams"]} >= {"video", "audio"}
        video_digest = hashlib.sha256(video.read_bytes()).hexdigest()
        print(f"PASS: ffmpeg generated playable {video_size}-byte MP4 with audio and video", flush=True)

        run(["docker", "build", "-t", helper_image, "-"], input_text=LIBTORRENT_DOCKERFILE, timeout=600)
        torrent = json.loads(run(["docker", "run", "--rm", "-v", f"{temporary}:/work",
                                  "-v", f"{make_torrent}:/make-torrent.py:ro", helper_image,
                                  "/make-torrent.py", "/work", object_id], capture=True, timeout=180))
        if not (torrent["size"] == video_size and torrent["pieces"] > 1 and len(torrent["info_hash_v2"]) == 64):
            raise AssertionError(f"invalid torrent metadata: size={torrent.get('size')} expected={video_size} pieces={torrent.get('pieces')} hash_length={len(torrent.get('info_hash_v2', ''))}")
        info_hash = torrent["info_hash_v2"]
        shutil.copyfile(temporary / "object.torrent", source / (object_id + ".torrent"))
        seed_grant = run(["go", "run", str(key_helper), "issue", str(private_key), info_hash],
                         capture=True, timeout=120)
        seed_url = "http://arknet-tracker:6969/announce/" + seed_grant
        seed_secret = temporary / "seed-secret.json"
        seed_secret.write_text(json.dumps({"tracker_url": seed_url}, separators=(",", ":")))
        os.chmod(seed_secret, 0o600); known_secrets.extend([seed_grant, seed_url, seed_secret.read_text()])
        print("PASS: generated private v2-only multi-piece torrent and dedicated seed-action grant", flush=True)

        run(["docker", "network", "create", network], capture=True); made_network = True
        core_override.write_text(json.dumps({
            "services": {
                "arknet-core": {"image": images["core"], "networks": ["core", "acceptance"]},
                "arknet-tracker": {"image": images["tracker"], "networks": ["tracker-edge", "acceptance"]},
            },
            "networks": {"acceptance": {"external": True, "name": network}},
        }))
        node_override.write_text(json.dumps({
            "services": {
                "arknet-agent": {"image": images["agent"], "networks": ["node", "media-services", "transfer-control", "acceptance"]},
                "arknet-transfer": {"image": images["transfer"], "networks": ["transfer-control", "acceptance"]},
                "jellyfin": {"image": "jellyfin/jellyfin:10.10.7", "volumes": [f"{jellyfin_config}:/config", f"{jellyfin_cache}:/cache", "media:/srv/arknet/media:ro"]},
            },
            "networks": {"acceptance": {"external": True, "name": network}},
        }))
        os.chmod(core_override, 0o600); os.chmod(node_override, 0o600)

        core_compose("up", "--build", "-d", "--wait", "--wait-timeout", "180")
        # Configure Jellyfin before starting Agent so its dedicated API key is never a placeholder.
        node_compose("up", "--build", "-d", "--wait", "--wait-timeout", "240",
                     "postgres", "jellyfin", "arknet-transfer")
        jellyfin_base = f"http://127.0.0.1:{jellyfin_port}"
        wait_until("Jellyfin HTTP", lambda: request(jellyfin_base + "/System/Info/Public")[0] == 200, 180)
        jellyfin_access, jellyfin_key = jellyfin_setup(jellyfin_base, jellyfin_password)
        known_secrets.extend([jellyfin_access, jellyfin_key])
        node_env["JELLYFIN_API_KEY"] = jellyfin_key
        node_compose("up", "-d", "--wait", "--wait-timeout", "180", "arknet-agent")
        core_base, agent_base = f"http://127.0.0.1:{core_port}", f"http://127.0.0.1:{agent_port}"

        def node_in_state(wanted):
            status, _, value = request_json(agent_base + "/api/v1/status")
            return value if status == 200 and value.get("state") == wanted else None
        node = wait_until("pending Node enrollment", lambda: node_in_state("ENROLLMENT_PENDING"), 120)
        core_admin = ScopedAdmin(core_compose, "transfer-stack", "nodes.decide")
        admin_headers = core_admin.headers()
        known_secrets.extend(core_admin.secrets())
        request_json(core_base + "/api/admin/v1/nodes/" + node["node_id"] + "/approve",
                     "POST", headers=admin_headers)
        wait_until("approved connected Node", lambda: node_in_state("CONNECTED"), 120)

        core_sql("INSERT INTO media_objects (public_id,content_type,title,size,state,info_hash_v2) VALUES "
                 f"('{object_id}','VIDEO','Prompt 25 playable acceptance',{video_size},'ACTIVE','{info_hash}')")
        run(["docker", "run", "-d", "--name", seeder_name, "--network", network,
             "-v", f"{temporary}:/work:ro", helper_image, "/work/seeder.py", "/work", object_id], capture=True)
        wait_until("authorized v2 metadata seed", lambda: "private_v2_seed" in run(
            ["docker", "logs", seeder_name], capture=True, check=False, timeout=10), 30)
        print("PASS: Core, tracker/web seed, authorized metadata seed, Node, real transfer and Jellyfin are live", flush=True)

        # This is the local API sequence behind the UI's one Make Available Locally action.
        _, _, selected = request_json(agent_base + f"/api/v1/media/{object_id}/selection", "POST", {})
        assert selected["local_state"] == "QUEUED"
        _, _, started = request_json(agent_base + f"/api/v1/media/{object_id}/transfer/start", "POST", {})
        if started["state"] != "DOWNLOADING" or started["mock"] is not False:
            layout = node_compose("exec", "-T", "arknet-transfer", "python3", "-c",
                                  "import json,pathlib; r=pathlib.Path('/srv/arknet/transfers'); print(json.dumps([str(p.relative_to(r)) for p in r.rglob('*')]))",
                                  capture=True, check=False, timeout=20).replace(object_id, "OBJECT")
            transfer_logs = node_compose("logs", "--tail", "20", "arknet-transfer", capture=True, check=False, timeout=20)
            raise AssertionError(f"real transfer did not start: state={started.get('state')} mock={started.get('mock')} error={started.get('error_code')} layout={layout} logs={transfer_logs}")
        deadline = time.time() + 180
        partial = None
        last_status = None
        while time.time() < deadline:
            last_status = transfer_status(agent_base, object_id)
            if last_status and 2 * 1024 * 1024 <= last_status["completed_bytes"] < video_size:
                partial = last_status
                break
            if last_status and last_status["completed_bytes"] >= video_size:
                raise AssertionError("transfer completed before the interruption could be injected")
            time.sleep(0.25)
        if partial is None:
            raise AssertionError(f"timed out waiting for measurable partial transfer; last={last_status!r}")
        partial_bytes = partial["completed_bytes"]
        time.sleep(2)  # allow the periodic sanitized fast-resume writer to commit
        print(f"PASS: operator path obtained Core grants and downloaded measurable partial bytes ({partial_bytes})", flush=True)

        # Remove every payload/metadata source, then recreate the transfer process.
        run(["docker", "stop", seeder_name], timeout=30)
        core_compose("stop", "arknet-core")
        before_restart = transfer_status(agent_base, object_id)["completed_bytes"]
        node_compose("restart", "arknet-transfer", timeout=120)
        wait_until("transfer API after restart", lambda: transfer_status(agent_base, object_id), 120)
        after_restart = transfer_status(agent_base, object_id)["completed_bytes"]
        if after_restart < before_restart - 256 * 1024:
            raise AssertionError(f"fastresume regressed from {before_restart} to {after_restart}")

        transfer_id = node_compose("ps", "-q", "arknet-transfer", capture=True)
        inspected = json.loads(run(["docker", "inspect", transfer_id], capture=True))[0]
        environment = dict(item.split("=", 1) for item in inspected["Config"]["Env"] if "=" in item)
        state_dir = environment["ARKNET_TRANSFER_STATE_DIR"]
        state_mount = next(m for m in inspected["Mounts"] if m["Destination"] == state_dir)
        state_scan = json.loads(run(["docker", "run", "--rm", "-v", state_mount["Name"] + ":/state:ro",
                                     helper_image, "-c", "import json,pathlib; files=list(pathlib.Path('/state').glob('*')); data=b''.join(p.read_bytes() for p in files if p.is_file()); print(json.dumps({'files':[p.name for p in files],'bytes':len(data),'has_url':b'/announce/' in data or b'/api/web-seed/' in data or b'/api/torrent/' in data}))"], capture=True))
        assert any(name.endswith(".fastresume") for name in state_scan["files"])
        assert state_scan["bytes"] > 0 and not state_scan["has_url"]
        print("PASS: source outage plus real transfer-container restart retained progress in credential-free fastresume", flush=True)

        core_compose("start", "arknet-core")
        wait_until("Core restored", lambda: request(core_base + "/health")[0] == 200, 90)
        run(["docker", "start", seeder_name])
        # Resume is idempotent when already downloading and forces fresh authorization delivery.
        status, _, resumed = request_json(agent_base + f"/api/v1/media/{object_id}/transfer/resume",
                                           "POST", {}, expected=(200, 409))
        if status == 409:
            # A reconciler may have completed the refresh between source restoration and this call.
            resumed = transfer_status(agent_base, object_id)
        available = wait_until("verified local AVAILABLE", lambda: (
            lambda listing: next((o for o in listing["objects"]
                                  if o["id"] == object_id and o["local_state"] == "AVAILABLE"), None)
        )(request_json(agent_base + "/api/v1/media")[2]), 300)
        assert available["size"] == video_size
        final_status = transfer_status(agent_base, object_id)
        assert final_status["state"] == "COMPLETED" and final_status["completed_bytes"] == video_size
        print("PASS: fresh authorization resumed verified pieces and finalized the object as AVAILABLE", flush=True)

        auth = {"X-Emby-Token": jellyfin_access}
        request_json(jellyfin_base + "/Library/Refresh", "POST", {}, auth, expected=(204,))
        item = wait_until("Jellyfin indexed imported video", lambda: (
            lambda result: next((entry for entry in result.get("Items", []) if entry.get("Type") == "Movie"), None)
        )(request_json(jellyfin_base + "/Items?Recursive=true&IncludeItemTypes=Movie&Fields=Path", headers=auth)[2]), 180)
        stream_url = jellyfin_base + "/Videos/" + item["Id"] + "/stream.mp4?" + urllib.parse.urlencode({
            "Static": "true", "api_key": jellyfin_key,
        })
        stream_status, _, streamed = request(stream_url, timeout=180)
        assert stream_status == 200 and len(streamed) == video_size
        assert hashlib.sha256(streamed).hexdigest() == video_digest
        print("PASS: Jellyfin indexed and streamed the exact completed playable MP4 bytes", flush=True)

        replica = core_sql("SELECT n.public_id||'|'||m.public_id||'|'||m.size||'|'||m.info_hash_v2||'|'||count(*) "
                           "FROM media_replicas r JOIN nodes n ON n.id=r.node_id "
                           "JOIN media_objects m ON m.id=r.media_object_id "
                           f"WHERE n.public_id='{node['node_id']}' AND m.public_id='{object_id}' "
                           "GROUP BY n.public_id,m.public_id,m.size,m.info_hash_v2")
        assert replica == f"{node['node_id']}|{object_id}|{video_size}|{info_hash}|1"
        assert core_sql(f"SELECT count(*) FROM media_replicas r JOIN media_objects m ON m.id=r.media_object_id WHERE m.public_id='{object_id}'") == "1"
        print("PASS: Core PostgreSQL has exactly one authenticated Node/object media_replicas row", flush=True)

        # Public/local API models and logs must not expose known credentials or host paths.
        safe_surfaces = []
        for url in (agent_base + "/api/v1/status", agent_base + "/api/v1/media", agent_base + "/api/v1/transfers"):
            safe_surfaces.append(request(url)[2].decode(errors="replace"))
        containers = json.loads(run(["docker", "ps", "-a", "--filter", f"label=com.docker.compose.project={core_project}",
                                     "--filter", f"label=com.docker.compose.project={node_project}", "--format", "{{json .}}"],
                                    capture=True) or "[]") if False else []
        # Docker combines repeated filters with AND, so enumerate each project separately.
        container_ids = []
        for project in (core_project, node_project):
            output = run(["docker", "ps", "-aq", "--filter", f"label=com.docker.compose.project={project}"], capture=True)
            container_ids.extend(output.splitlines())
        container_ids.append(seeder_name)
        log_chunks = []
        for cid in container_ids:
            if not cid:
                continue
            result = subprocess.run(["docker", "logs", cid], cwd=ROOT, text=True,
                                    capture_output=True, timeout=20, check=False)
            log_chunks.append(result.stdout + result.stderr)
        logs = "\n".join(log_chunks)
        exposed = "\n".join(safe_surfaces) + "\n" + logs
        prohibited = known_secrets + [str(temporary), seed_url]
        leaked = [value for value in prohibited if value and value in exposed]
        if leaked:
            raise AssertionError(f"known credential/path leaked on API/log surfaces ({len(leaked)} values)")
        assert "/srv/arknet" not in "\n".join(safe_surfaces)
        print("PASS: Agent APIs and container logs expose no known token, grant, API key, password, or path", flush=True)
    except Exception as error:
        failure = error
        # Keep diagnostics narrow and redact every value known to this process.
        try:
            diagnostics = []
            for project, compose in ((core_project, core_compose), (node_project, node_compose)):
                text = compose("logs", "--tail", "40", capture=True, check=False, timeout=30)
                diagnostics.append(project + ":\n" + text)
            diagnostic = "\n".join(diagnostics)
            for value in sorted((v for v in known_secrets if v), key=len, reverse=True):
                diagnostic = diagnostic.replace(value, "[REDACTED]")
            diagnostic = diagnostic.replace(str(temporary), "[TEMPORARY]")
            if diagnostic.strip():
                print("Sanitized diagnostics:\n" + diagnostic[-12000:], flush=True)
        except Exception:
            pass
        raise
    finally:
        run(["docker", "rm", "-f", seeder_name], check=False, timeout=30)
        node_compose("down", "--volumes", "--remove-orphans", check=False, timeout=180)
        core_compose("down", "--volumes", "--remove-orphans", check=False, timeout=180)
        if made_network:
            run(["docker", "network", "rm", network], check=False, timeout=30)
        for image in images.values():
            run(["docker", "image", "rm", "-f", image], check=False, timeout=60)
        shutil.rmtree(temporary, ignore_errors=False)

        leftovers = []
        for project in (core_project, node_project):
            for kind, command in (("container", ["docker", "ps", "-aq", "--filter", f"label=com.docker.compose.project={project}"]),
                                  ("volume", ["docker", "volume", "ls", "-q", "--filter", f"label=com.docker.compose.project={project}"]),
                                  ("network", ["docker", "network", "ls", "-q", "--filter", f"label=com.docker.compose.project={project}"])):
                output = run(command, capture=True, check=False, timeout=30)
                if output:
                    leftovers.append(f"{kind}:{project}:{output.replace(chr(10), ',')}")
        for kind, name in (("container", seeder_name), ("network", network)):
            if subprocess.run(["docker", kind, "inspect", name], cwd=ROOT,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              timeout=20, check=False).returncode == 0:
                leftovers.append(kind + ":" + name)
        for image in images.values():
            if subprocess.run(["docker", "image", "inspect", image], cwd=ROOT,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              timeout=20, check=False).returncode == 0:
                leftovers.append("image:" + image)
        if temporary.exists():
            leftovers.append("directory:" + str(temporary))
        if leftovers:
            cleanup_error = RuntimeError("exact cleanup verification failed: " + ", ".join(leftovers))
            if failure is None:
                raise cleanup_error
            print("ERROR: " + str(cleanup_error), flush=True)
        else:
            print("PASS: exact scoped cleanup removed this run's containers, networks, volumes, images and secrets", flush=True)


if __name__ == "__main__":
    main()
