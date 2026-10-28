#!/usr/bin/env python3
"""Prove Core's private v2 HTTP web seed with an isolated libtorrent client.

The check creates only uniquely named Docker resources and removes every exact
resource plus its credential-bearing temporary directory in ``finally``.
"""
import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent

GO_HELPER = r'''
package main

import (
    "crypto/ed25519"
    "crypto/rand"
    "crypto/sha256"
    "encoding/base64"
    "encoding/binary"
    "encoding/hex"
    "encoding/json"
    "os"
    "path/filepath"
    "time"
)

func main() {
    if len(os.Args) != 3 { panic("usage: helper DIRECTORY INFO_HASH_V2") }
    hash, err := hex.DecodeString(os.Args[2]); if err != nil || len(hash) != 32 { panic("bad hash") }
    _, privateKey, err := ed25519.GenerateKey(rand.Reader); if err != nil { panic(err) }
    keyPath := filepath.Join(os.Args[1], "tracker-private.key")
    encodedKey := base64.StdEncoding.EncodeToString(privateKey)
    if err = os.WriteFile(keyPath, []byte(encodedKey), 0600); err != nil { panic(err) }
    issue := func(nodeID string, actions byte, expiry time.Time, grantHash []byte) string {
        payload := make([]byte, 58)
        payload[0] = 2
        binary.BigEndian.PutUint64(payload[1:9], uint64(expiry.Unix()))
        subjectMessage := append([]byte("arknet-tracker-subject-v1\x00"+nodeID+"\x00"), grantHash...)
        subject := sha256.Sum256(ed25519.Sign(privateKey, subjectMessage))
        copy(payload[9:25], subject[:16])
        copy(payload[25:57], grantHash)
        payload[57] = actions
        signature := ed25519.Sign(privateKey, payload)
        return base64.RawURLEncoding.EncodeToString(payload)+"."+base64.RawURLEncoding.EncodeToString(signature)
    }
    wrongHash := make([]byte, 32); if _, err = rand.Read(wrongHash); err != nil { panic(err) }
    now := time.Now().UTC()
    result := map[string]string{
        "download": issue("acceptance-download", 1, now.Add(5*time.Minute), hash),
        "seed": issue("acceptance-seed", 2, now.Add(5*time.Minute), hash),
        "wrong_swarm": issue("acceptance-wrong-swarm", 1, now.Add(5*time.Minute), wrongHash),
        "expired": issue("acceptance-expired", 1, now.Add(-time.Minute), hash),
    }
    if err = json.NewEncoder(os.Stdout).Encode(result); err != nil { panic(err) }
}
'''

TORRENT_HELPER = r'''
import json
import os
from pathlib import Path
import sys
import libtorrent as lt

work = Path(sys.argv[1])
object_id = sys.argv[2]
source = work / "source" / object_id
size = source.stat().st_size
files = lt.file_storage()
files.add_file(object_id, size)
creator = lt.create_torrent(files, 256 * 1024, lt.create_torrent.v2_only)
creator.set_priv(True)
creator.add_tracker("http://127.0.0.1:1/announce")
lt.set_piece_hashes(creator, str(source.parent))
encoded = lt.bencode(creator.generate())
(work / "object.torrent").write_bytes(encoded)
info = lt.torrent_info(encoded)
hashes = info.info_hashes()
assert info.priv() and hashes.has_v2() and not hashes.has_v1()
print(json.dumps({"hash": str(hashes.v2).lower(), "size": info.total_size()}))
'''

CLIENT = r'''
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import time
import libtorrent as lt

work = Path(sys.argv[1])
object_id = sys.argv[2]
expected_hash = sys.argv[3]
expected_size = int(sys.argv[4])
config = json.loads((work / "client-secret.json").read_text())
spec = importlib.util.spec_from_file_location("arknet_worker", "/opt/arknet-worker.py")
worker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(worker)
info = lt.torrent_info(str(work / "object.torrent"))
assert worker.valid_metadata(info, expected_hash, expected_size)
params = worker.new_params(object_id, expected_size, expected_hash,
                           "http://127.0.0.1:1/announce", work / "download",
                           config["web_seed_url"])
params.ti = info
settings = worker.session_settings("0.0.0.0:0", 0, True)
# The acceptance origin is fixed to the isolated Docker service. Libtorrent's
# local-address SSRF guard is relaxed only in this disposable client session.
settings["alert_mask"] = int(lt.alert.category_t.all_categories)
session = lt.session(settings)
session.add_extension(lt.create_ut_metadata_plugin)
handle = session.add_torrent(params)
deadline = time.monotonic() + 90
saw_web_seed = False
saw_range_log = False
while time.monotonic() < deadline:
    for peer in handle.get_peer_info():
        saw_web_seed = saw_web_seed or peer.connection_type == lt.peer_info.web_seed
    for alert in session.pop_alerts():
        message = alert.message().lower()
        # Never emit alert text: it can contain the credential-bearing URL.
        saw_range_log = saw_range_log or ("range:" in message and "bytes=" in message)
    status = handle.status()
    if status.is_seeding:
        break
    error_value = status.errc.value() if hasattr(status.errc, "value") else 0
    if error_value:
        message = status.errc.message() if hasattr(status.errc, "message") else "nonzero error"
        raise RuntimeError("libtorrent error: " + message)
    time.sleep(0.05)
else:
    raise RuntimeError("timed out waiting for web-seed transfer")
output = work / "download" / object_id / object_id
assert output.is_file() and output.stat().st_size == expected_size
assert hashlib.sha256(output.read_bytes()).hexdigest() == config["content_sha256"]
status = handle.status()
assert status.total_payload_download >= expected_size
assert saw_web_seed or status.num_seeds > 0
print(json.dumps({"bytes": status.total_payload_download,
                  "web_seed_observed": saw_web_seed,
                  "range_log_observed": saw_range_log}))
'''

CLIENT_DOCKERFILE = """FROM ubuntu:24.04
RUN apt-get update -qq \\
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends python3 python3-libtorrent ca-certificates \\
 && rm -rf /var/lib/apt/lists/*
ENTRYPOINT [\"python3\"]
"""


def run(command, *, capture=False, input_text=None, timeout=300):
    result = subprocess.run(command, cwd=ROOT, check=True, text=True,
                            input=input_text, capture_output=capture, timeout=timeout)
    return result.stdout.strip() if capture else ""


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def request(url, method="GET", headers=None):
    req = urllib.request.Request(url, method=method, headers=headers or {})
    try:
        response = urllib.request.urlopen(req, timeout=10)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, dict(response.headers), response.read()


def wait_until(description, probe, seconds=60):
    deadline = time.monotonic() + seconds
    last_error = None
    while time.monotonic() < deadline:
        try:
            if probe():
                return
        except Exception as error:  # transient startup errors are expected
            last_error = error
        time.sleep(0.25)
    raise RuntimeError(f"timed out waiting for {description}: {last_error}")


def main():
    prefix = "arknet-webseed-test-" + uuid.uuid4().hex[:12]
    names = {
        "network": prefix + "-net",
        "volume": prefix + "-pgdata",
        "postgres": prefix + "-postgres",
        "core": prefix + "-core",
        "client": prefix + "-client",
        "core_image": prefix + "-core-image",
        "client_image": prefix + "-client-image",
    }
    temporary = Path(tempfile.mkdtemp(prefix="." + prefix + "-", dir=ROOT))
    # Core runs as UID 10001. Permit traversal only for this disposable mount;
    # credential-bearing client state remains mode 0600 and is always removed.
    os.chmod(temporary, 0o755)
    credentials = []
    failure = None
    try:
        source = temporary / "source"
        download = temporary / "download"
        web_root = temporary / "web-seed"
        source.mkdir(mode=0o755)
        download.mkdir(mode=0o700)
        web_root.mkdir(mode=0o755)
        object_id = "media_" + secrets.token_hex(32)
        # Deterministic, incompressible-looking bytes spanning many pieces/ranges.
        content = b"".join(hashlib.sha256(f"arknet-web-seed-{i}".encode()).digest()
                           for i in range(262145))
        # Keep the v2 single-file length piece-aligned so libtorrent does not
        # add a synthetic pad file to torrent_info.total_size().
        content = content[:8 * 1024 * 1024]
        source_file = source / object_id
        source_file.write_bytes(content)
        os.chmod(source_file, 0o644)
        shutil.copyfile(source_file, web_root / object_id)
        os.chmod(web_root / object_id, 0o644)
        content_sha256 = hashlib.sha256(content).hexdigest()

        torrent_helper = temporary / "make-torrent.py"
        client_helper = temporary / "client.py"
        go_helper = temporary / "grant-helper.go"
        torrent_helper.write_text(TORRENT_HELPER)
        client_helper.write_text(CLIENT)
        go_helper.write_text(GO_HELPER)
        for helper in (torrent_helper, client_helper, go_helper):
            os.chmod(helper, 0o600)

        run(["docker", "build", "-t", names["client_image"], "-"],
            input_text=CLIENT_DOCKERFILE, timeout=600)
        torrent_result = json.loads(run([
            "docker", "run", "--rm", "-v", f"{temporary}:/work",
            "-v", f"{torrent_helper}:/make-torrent.py:ro", names["client_image"],
            "/make-torrent.py", "/work", object_id], capture=True, timeout=120))
        info_hash = torrent_result["hash"]
        size = torrent_result["size"]
        assert size == len(content), f"torrent size {size} != source size {len(content)}"
        assert len(info_hash) == 64, f"unexpected v2 hash representation length {len(info_hash)}"
        print("PASS: generated deterministic private v2-only single-file torrent", flush=True)

        grants = json.loads(run(["go", "run", str(go_helper), str(temporary), info_hash],
                                capture=True, timeout=120))
        credentials.extend(grants.values())
        key_file = temporary / "tracker-private.key"
        os.chmod(key_file, 0o644)
        assert key_file.stat().st_mode & 0o777 == 0o644
        print("PASS: generated dedicated Ed25519 key and canonical v2 grants", flush=True)

        run(["docker", "build", "-f", "core/Dockerfile", "-t", names["core_image"], "core"],
            timeout=600)
        run(["docker", "network", "create", names["network"]], capture=True)
        run(["docker", "volume", "create", names["volume"]], capture=True)
        database_password = secrets.token_hex(32)
        credentials.append(database_password)
        run([
            "docker", "run", "-d", "--name", names["postgres"],
            "--network", names["network"], "--network-alias", "postgres",
            "-e", "POSTGRES_USER=arknet", "-e", "POSTGRES_DB=arknet_core",
            "-e", "POSTGRES_PASSWORD=" + database_password,
            "-v", names["volume"] + ":/var/lib/postgresql",
            "postgres:18-alpine"], capture=True)
        postgres_ready = lambda: subprocess.run([
            "docker", "exec", names["postgres"], "psql", "-U", "arknet",
            "-d", "arknet_core", "-At", "-c", "SELECT 1"], stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL).returncode == 0
        # PostgreSQL's first-init temporary server can briefly accept checks
        # before the entrypoint restarts it. Require readiness again afterward.
        wait_until("PostgreSQL initialization", postgres_ready)
        time.sleep(1)
        wait_until("stable PostgreSQL", postgres_ready)

        port = free_port()
        database_url = ("postgresql://arknet:" + database_password +
                        "@postgres:5432/arknet_core?sslmode=disable")
        credentials.append(database_url)
        run([
            "docker", "run", "-d", "--name", names["core"],
            "--network", names["network"], "--network-alias", "arknet-core",
            "-p", f"127.0.0.1:{port}:8080",
            "-e", "ARKNET_CORE_ADDR=0.0.0.0:8080",
            "-e", "DATABASE_URL=" + database_url,
            "-e", "ARKNET_TRACKER_ANNOUNCE_URL=http://127.0.0.1:1",
            "-e", "ARKNET_TRACKER_SIGNING_KEY_FILE=/run/secrets/tracker-private-key",
            "-e", "ARKNET_CORE_WEB_SEED_ORIGIN=http://arknet-core:8080",
            "-e", "ARKNET_CORE_WEB_SEED_ROOT=/srv/arknet-web-seed",
            "-v", f"{key_file}:/run/secrets/tracker-private-key:ro",
            "-v", f"{web_root}:/srv/arknet-web-seed:ro",
            names["core_image"]], capture=True)
        host_base = f"http://127.0.0.1:{port}"
        wait_until("Core", lambda: request(host_base + "/health")[0] == 200)
        print("PASS: disposable PostgreSQL and Core containers are healthy", flush=True)

        sql = ("INSERT INTO media_objects "
               "(public_id,content_type,title,size,state,info_hash_v2) VALUES "
               f"('{object_id}','VIDEO','Web seed acceptance',{size},'ACTIVE','{info_hash}')")
        run(["docker", "exec", names["postgres"], "psql", "-v", "ON_ERROR_STOP=1",
             "-U", "arknet", "-d", "arknet_core", "-c", sql], capture=True)
        path = "/api/web-seed/v1/media/" + object_id + "/"
        authorized = host_base + path + grants["download"]

        status, headers, body = request(authorized)
        assert status == 200 and body == content
        assert headers.get("Cache-Control") == "no-store"
        status, headers, body = request(authorized, method="HEAD")
        assert status == 200 and body == b"" and int(headers["Content-Length"]) == size
        status, headers, body = request(authorized, headers={"Range": "bytes=1048576-1048607"})
        assert status == 206 and body == content[1048576:1048608]
        assert headers.get("Content-Range") == f"bytes 1048576-1048607/{size}"
        status, headers, _ = request(authorized, headers={"Range": f"bytes={size + 1}-{size + 9}"})
        assert status == 416 and headers.get("Content-Range") == f"bytes */{size}"
        print("PASS: Core serves full GET, HEAD, byte range 206, and unsatisfiable 416", flush=True)

        denied = [
            host_base + path + "not-a-grant",
            host_base + "/api/web-seed/v1/media/" + object_id,
            host_base + path + grants["seed"],
            host_base + path + grants["wrong_swarm"],
            host_base + path + grants["expired"],
        ]
        for url in denied:
            denied_status, _, denied_body = request(url)
            assert denied_status in (403, 404) and denied_body != content
        unsafe = [
            authorized + "/extra",
            host_base + "/api/web-seed/v1/media/not-an-id/" + grants["download"],
            host_base + "/api/web-seed/v1/media/%2e%2e/%2e%2e/etc/passwd/" + grants["download"],
            authorized + "?download=true",
        ]
        for url in unsafe:
            unsafe_status, _, unsafe_body = request(url)
            assert unsafe_status != 200 and unsafe_body != content
        print("PASS: missing/malformed/scoped/expired grants and arbitrary paths are denied", flush=True)

        client_secret = temporary / "client-secret.json"
        client_secret.write_text(json.dumps({
            "web_seed_url": "http://arknet-core:8080" + path + grants["download"],
            "content_sha256": content_sha256,
        }, separators=(",", ":")))
        os.chmod(client_secret, 0o600)
        credentials.append(client_secret.read_text())
        before_stats = json.loads(run(["docker", "stats", "--no-stream", "--format",
                                       "{{json .}}", names["core"]], capture=True))
        client_output = json.loads(run([
            "docker", "run", "--name", names["client"],
            "--network", names["network"],
            "-v", f"{temporary}:/work",
            "-v", f"{client_helper}:/client.py:ro",
            "-v", f"{ROOT / 'transfer' / 'worker.py'}:/opt/arknet-worker.py:ro",
            names["client_image"], "/client.py", "/work", object_id, info_hash, str(size),
        ], capture=True, timeout=150))
        assert client_output["bytes"] >= size
        after_stats = json.loads(run(["docker", "stats", "--no-stream", "--format",
                                      "{{json .}}", names["core"]], capture=True))
        # Completion with DHT/LSD/PEX disabled, a dead tracker, and exactly one URL
        # seed proves the payload source. Core's independently verified 206 response
        # proves that source's range semantics; network counters provide a live
        # cross-check that Core transmitted during the libtorrent run.
        assert before_stats["NetIO"] != after_stats["NetIO"]
        downloaded = temporary / "download" / object_id / object_id
        assert hashlib.sha256(downloaded.read_bytes()).hexdigest() == content_sha256
        print(f"PASS: Ubuntu 24.04 libtorrent retrieved and hash-verified {size} bytes via Core ranges", flush=True)

        logs = run(["docker", "logs", names["core"]], capture=True)
        assert all(secret not in logs for secret in credentials)
        assert grants["download"] not in logs and object_id not in logs
        print("PASS: Core logs contain no token, credential, media ID, or request URL", flush=True)
    except Exception as error:
        failure = error
        try:
            diagnostics = []
            for container in (names["client"], names["core"], names["postgres"]):
                exists = subprocess.run(["docker", "container", "inspect", container],
                                        cwd=ROOT, stdout=subprocess.DEVNULL,
                                        stderr=subprocess.DEVNULL).returncode == 0
                if exists:
                    logs = subprocess.run(["docker", "logs", "--tail", "50", container],
                                          cwd=ROOT, text=True, capture_output=True, timeout=10)
                    diagnostics.append(logs.stdout + logs.stderr)
            diagnostic = "\n".join(diagnostics)
            if diagnostic:
                for secret in credentials:
                    diagnostic = diagnostic.replace(secret, "[REDACTED]")
                if diagnostic.strip():
                    print("Core diagnostic (credentials redacted):\n" + diagnostic.strip(), flush=True)
        except Exception:
            pass
        raise
    finally:
        # No prune/down wildcard: remove only exact resources created by this run.
        for container in (names["client"], names["core"], names["postgres"]):
            subprocess.run(["docker", "rm", "-f", container], cwd=ROOT,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["docker", "network", "rm", names["network"]], cwd=ROOT,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["docker", "volume", "rm", "-f", names["volume"]], cwd=ROOT,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for image in (names["core_image"], names["client_image"]):
            subprocess.run(["docker", "image", "rm", "-f", image], cwd=ROOT,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        shutil.rmtree(temporary, ignore_errors=False)
        leftovers = []
        for kind, name in (("container", names["client"]), ("container", names["core"]),
                           ("container", names["postgres"]), ("network", names["network"]),
                           ("volume", names["volume"]), ("image", names["core_image"]),
                           ("image", names["client_image"])):
            if subprocess.run(["docker", kind, "inspect", name], cwd=ROOT,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
                leftovers.append(kind + ":" + name)
        if temporary.exists():
            leftovers.append("directory:" + str(temporary))
        if leftovers:
            cleanup_error = RuntimeError("cleanup failed for " + ", ".join(leftovers))
            if failure is None:
                raise cleanup_error
            print("ERROR: " + str(cleanup_error), flush=True)
        else:
            print("PASS: removed exact disposable containers, network, volume, images, and secrets", flush=True)


if __name__ == "__main__":
    main()
