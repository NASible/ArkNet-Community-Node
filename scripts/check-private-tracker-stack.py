#!/usr/bin/env python3
"""Exercise an isolated private tracker with Core-format signed grants."""
import base64
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent

KEY_HELPER = r'''
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
  publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader); if err != nil { panic(err) }
  directory := os.Args[1]
  if err = os.WriteFile(filepath.Join(directory, "private.key"), []byte(base64.StdEncoding.EncodeToString(privateKey)), 0600); err != nil { panic(err) }
  if err = os.WriteFile(filepath.Join(directory, "public.key"), []byte(base64.StdEncoding.EncodeToString(publicKey)), 0644); err != nil { panic(err) }
  hash := make([]byte, 32); if _, err = rand.Read(hash); err != nil { panic(err) }
  issue := func(nodeID string, actions byte, expiry time.Time) string {
    payload := make([]byte, 58); payload[0] = 2
    binary.BigEndian.PutUint64(payload[1:9], uint64(expiry.Unix()))
    subjectMessage := append([]byte("arknet-tracker-subject-v1\x00"+nodeID+"\x00"), hash...)
    subject := sha256.Sum256(ed25519.Sign(privateKey, subjectMessage)); copy(payload[9:25], subject[:16]); copy(payload[25:57], hash); payload[57] = actions
    return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
  }
  future := time.Now().Add(5*time.Minute)
  expired := time.Now().Add(-time.Second)
  if err = json.NewEncoder(os.Stdout).Encode(map[string]string{
    "token_a": issue("node-a", 3, future), "token_b": issue("node-b", 3, future),
    "download_token": issue("node-download", 1, future), "seed_token": issue("node-seed", 2, future),
    "expired_token": issue("node-expired", 3, expired), "hash": hex.EncodeToString(hash),
  }); err != nil { panic(err) }
}
'''


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def bdecode(data):
    def parse(offset):
        if data[offset:offset + 1] == b"i":
            end = data.index(b"e", offset)
            return int(data[offset + 1:end]), end + 1
        if data[offset:offset + 1] == b"d":
            result, offset = {}, offset + 1
            while data[offset:offset + 1] != b"e":
                key, offset = parse(offset)
                value, offset = parse(offset)
                result[key] = value
            return result, offset + 1
        colon = data.index(b":", offset)
        length = int(data[offset:colon])
        start = colon + 1
        return data[start:start + length], start + length
    result, end = parse(0)
    assert end == len(data)
    return result


def main():
    prefix = "arknet-tracker-test-" + uuid.uuid4().hex[:12]
    image = prefix
    container = prefix
    port = free_port()
    with tempfile.TemporaryDirectory(prefix="." + prefix + "-", dir=ROOT) as directory:
        root = Path(directory)
        helper = root / "keys.go"
        helper.write_text(KEY_HELPER)
        os.chmod(helper, 0o600)
        issued = json.loads(subprocess.run(
            ["go", "run", str(helper), str(root)], check=True, text=True,
            capture_output=True).stdout)
        token_a, token_b, full_hash = issued["token_a"], issued["token_b"], issued["hash"]
        download_token, seed_token, expired_token = issued["download_token"], issued["seed_token"], issued["expired_token"]
        hash_bytes = bytes.fromhex(full_hash)
        public_key = root / "public.key"

        subprocess.run(["docker", "build", "-t", image, str(ROOT / "tracker")], check=True)
        try:
            subprocess.run([
                "docker", "run", "-d", "--name", container,
                "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges:true",
                "-p", f"127.0.0.1:{port}:6969",
                "-e", "ARKNET_TRACKER_BIND_ADDR=0.0.0.0:6969",
                "-e", "ARKNET_TRACKER_VERIFY_KEY_FILE=/run/secrets/tracker-public-key",
                "-e", "ARKNET_TRACKER_ANNOUNCE_INTERVAL=60s",
                "-e", "ARKNET_TRACKER_PEER_TTL=90s",
                "-v", f"{public_key}:/run/secrets/tracker-public-key:ro",
                image,
            ], check=True, stdout=subprocess.DEVNULL)
            base = f"http://127.0.0.1:{port}"
            deadline = time.monotonic() + 30
            while True:
                try:
                    with urllib.request.urlopen(base + "/health", timeout=2) as response:
                        assert response.status == 200 and response.read() == b"ok\n"
                    break
                except (OSError, urllib.error.URLError):
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(0.2)

            deadline = time.monotonic() + 30
            while True:
                health = subprocess.run(
                    ["docker", "inspect", "--format", "{{.State.Health.Status}}", container],
                    check=True, text=True, capture_output=True).stdout.strip()
                if health == "healthy":
                    break
                if health == "unhealthy" or time.monotonic() >= deadline:
                    raise AssertionError("container health check did not become healthy: " + health)
                time.sleep(0.2)

            def query(info_hash, peer_id, peer_port, event="started", left=1):
                return urllib.parse.urlencode({
                    "info_hash": info_hash, "peer_id": peer_id, "port": str(peer_port),
                    "uploaded": "0", "downloaded": "0", "left": str(left), "corrupt": "0",
                    "key": "0123ABCD", "event": event, "numwant": "200", "compact": "1",
                    "no_peer_id": "1", "supportcrypto": "1", "redundant": "0",
                })

            def announce(grant, info_hash, peer_id, peer_port, event="started", left=1):
                url = base + "/announce/" + grant + "?" + query(info_hash, peer_id, peer_port, event, left)
                try:
                    response = urllib.request.urlopen(url, timeout=5)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    return response.status, response.headers, response.read()

            peer_a = b"-ARKNET-A-0000000001"
            peer_b = b"-ARKNET-B-0000000002"
            assert len(peer_a) == len(peer_b) == 20

            status, _, body = announce("not-a-grant", hash_bytes[:20], peer_a, 6881)
            assert status == 403 and b"invalid grant" in body
            status, _, body = announce(token_a, secrets.token_bytes(20), peer_a, 6881)
            assert status == 400 and b"invalid announce" in body
            status, _, body = announce(expired_token, hash_bytes[:20], peer_a, 6881)
            assert status == 403 and b"invalid grant" in body
            print("PASS: endpoint, hash, and expired authorization cannot join", flush=True)

            peer_download = b"-ARKNET-D-0000000003"
            peer_seed = b"-ARKNET-S-0000000004"
            assert len(peer_download) == len(peer_seed) == 20
            assert announce(download_token, hash_bytes[:20], peer_download, 6883, left=0)[0] == 403
            assert announce(seed_token, hash_bytes[:20], peer_seed, 6884, left=1)[0] == 403
            assert announce(download_token, hash_bytes[:20], peer_download, 6883, left=1)[0] == 200
            assert announce(seed_token, hash_bytes[:20], peer_seed, 6884, left=0)[0] == 200
            assert announce(download_token, hash_bytes[:20], peer_download, 6883, "stopped", left=1)[0] == 200
            assert announce(seed_token, hash_bytes[:20], peer_seed, 6884, "stopped", left=0)[0] == 200
            print("PASS: download and seed action scopes are enforced", flush=True)

            status, headers, body = announce(token_a, hash_bytes[:20], peer_a, 6881)
            first = bdecode(body)
            assert status == 200 and headers["Cache-Control"] == "no-store" and first[b"peers"] == b""
            status, _, body = announce(token_b, hash_bytes[:20], peer_b, 6882)
            second = bdecode(body)
            assert status == 200 and len(second[b"peers"]) == 6
            assert int.from_bytes(second[b"peers"][-2:], "big") == 6881
            assert second[b"incomplete"] == 2 and second[b"complete"] == 0
            print("PASS: two authorized v2 peers join and receive compact private-swarm peers", flush=True)

            assert announce(token_b, hash_bytes[:20], peer_a, 6999)[0] == 409
            assert announce(token_b, hash_bytes[:20], peer_a, 6999, "stopped")[0] == 409
            print("PASS: one authorized subject cannot overwrite or stop another subject's peer", flush=True)

            assert announce(token_a, hash_bytes[:20], peer_a, 6881, "stopped")[0] == 200
            status, _, body = announce(token_b, hash_bytes[:20], peer_b, 6882)
            assert status == 200 and bdecode(body)[b"peers"] == b""
            print("PASS: stopped peers are removed from the in-memory private swarm", flush=True)

            inspected = json.loads(subprocess.run(
                ["docker", "inspect", container], check=True, text=True,
                capture_output=True).stdout)[0]
            environment = inspected["Config"]["Env"]
            assert not any(name.startswith(("DATABASE_URL=", "PG", "ARKNET_CORE_")) for name in environment)
            mounts = inspected["Mounts"]
            assert len(mounts) == 1 and mounts[0]["Destination"] == "/run/secrets/tracker-public-key" and not mounts[0]["RW"]
            log_result = subprocess.run(["docker", "logs", container], check=True, text=True,
                                        capture_output=True)
            logs = log_result.stdout + log_result.stderr
            for secret_value in (token_a, token_b, download_token, seed_token, expired_token,
                                 full_hash, peer_a.decode(), peer_b.decode(), peer_download.decode(), peer_seed.decode()):
                assert secret_value not in logs
            print("PASS: tracker has only the public key, no Core database credentials, and minimal logs", flush=True)
        except Exception:
            subprocess.run(["docker", "logs", "--tail", "50", container], check=False)
            raise
        finally:
            subprocess.run(["docker", "rm", "-f", container], check=False,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(["docker", "image", "rm", image], check=False,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            print("Removed only disposable tracker resources: " + prefix, flush=True)


if __name__ == "__main__":
    main()
