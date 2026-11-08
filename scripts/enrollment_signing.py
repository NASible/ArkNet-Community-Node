"""Signed disposable enrollment probes (stdlib + OpenSSL 3, no Python deps).

One signer retains a distinct ephemeral key per node until closed. Never print
private keys; OpenSSL receives them through stdin or a mode-0600 temporary file,
never argv. Requests are paced, including intentionally invalid submissions;
production admission limits must stay enabled. Use one signer per harness.
"""
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
import urllib.parse
import urllib.request

from scripts import harness_scratch


def temporary_root():
    return str(harness_scratch.scratch_root())


class EnrollmentSigner:
    def __init__(self, interval=7.5):
        # Homebrew OpenSSL is needed on Macs shipping LibreSSL without Ed25519.
        self.openssl = (os.environ.get("ARKNET_TEST_OPENSSL") or
                        next((p for p in ("/opt/homebrew/opt/openssl@3/bin/openssl",
                                           "/usr/local/opt/openssl@3/bin/openssl") if Path(p).is_file()), None) or
                        shutil.which("openssl"))
        if not self.openssl:
            raise RuntimeError("OpenSSL 3 is required (or set ARKNET_TEST_OPENSSL)")
        self._keys = {}
        self._interval = interval
        self._lock = threading.Lock()
        self._next = 0.0

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self._keys.clear()

    def _run(self, *args, data=None):
        result = subprocess.run([self.openssl, *args], input=data, capture_output=True)
        if result.returncode:
            # Do not attach stdout/stderr or input to exceptions/logs.
            raise RuntimeError("Enrollment cryptography failed; OpenSSL 3 with Ed25519 support is required")
        return result.stdout

    def request(self, endpoint, node_id, version, extra=None):
        with self._lock:
            delay = self._next - time.monotonic()
            if delay > 0:
                time.sleep(delay)
            self._next = time.monotonic() + self._interval
            if node_id not in self._keys:
                private = self._run("genpkey", "-algorithm", "ED25519")
                public = self._run("pkey", "-pubout", "-outform", "DER", data=private)[-32:]
                self._keys[node_id] = (private, base64.b64encode(public).decode("ascii"))
            private, public = self._keys[node_id]
            body = {"node_id": node_id, "version": version, "capabilities": {}, "public_key": public}
            if extra:
                body.update(extra)  # Only for deliberate malformed-payload probes.
            data = json.dumps(body, separators=(",", ":")).encode("utf-8")
            timestamp = str(int(time.time()))
            path = urllib.parse.urlsplit(endpoint).path or "/"
            message = ("ARKNET-NODE-V1\nPOST\n" + path + "\n" + hashlib.sha256(data).hexdigest() +
                       "\n" + timestamp + "\n" + node_id).encode("utf-8")
            with tempfile.TemporaryDirectory(prefix="arknet-enrollment-", dir=temporary_root()) as directory:
                root = Path(directory)
                for name, content in (("key.pem", private), ("message", message)):
                    fd = os.open(str(root / name), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                    with os.fdopen(fd, "wb") as handle:
                        handle.write(content)
                signature = self._run("pkeyutl", "-sign", "-rawin", "-inkey", str(root / "key.pem"),
                                      "-in", str(root / "message"))
            return urllib.request.Request(endpoint, data=data, method="POST", headers={
                "Content-Type": "application/json", "X-ArkNet-Node-Timestamp": timestamp,
                "X-ArkNet-Node-Signature": base64.b64encode(signature).decode("ascii")})
