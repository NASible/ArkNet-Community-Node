#!/usr/bin/env python3
"""Acceptance check: a fresh Ubuntu LTS VM reaches ENROLLMENT_PENDING by
following docs/node-installation.md.

Host side (this machine, Docker + Lima + Go required):
  * ArkNet Core (built from core/) with a disposable PostgreSQL, on loopback;
  * a throwaway image registry holding version-stamped ArkNet 0.1.0 images;
  * a release site: install.sh, arknet-node, index.json, releases/0.1.0/.

VM side: a brand-new Lima Ubuntu 24.04 VM with no Docker and no host mounts.
Only the documented commands are run in it. The VM reaches the host through
Lima's gateway 192.168.5.2 (which maps to host loopback); the registry is
tunnelled to the VM's localhost so Docker needs no insecure-registry setup.

Everything created is removed at the end (use --keep-vm to inspect the VM).
"""
import functools
import hashlib
import http.server
import json
import os
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
NODE_DIR = ROOT / "deploy" / "node"
VM = "arknet-install-test"
GATEWAY = "192.168.5.2"
CORE_PORT, SITE_PORT, REGISTRY_PORT, PG_PORT = 18120, 18121, 15041, 55442
REGISTRY = f"localhost:{REGISTRY_PORT}"
VERSION = "0.1.0"
PG_NAME, REGISTRY_NAME = "arknet-install-test-pg", "arknet-install-test-registry"
results = {}


def sh(*args, check=True, **kw):
    return subprocess.run(list(args), check=check, capture_output=True, text=True, **kw)


def vm(command, check=False, timeout=1800):
    return subprocess.run(["limactl", "shell", "--workdir", "/tmp", VM, "--", "bash", "-lc", command],
                          capture_output=True, text=True, timeout=timeout, check=check)


def check(name, condition, detail=""):
    results[name] = bool(condition)
    print(("PASS " if condition else "FAIL ") + name + (f" ({detail})" if detail else ""), flush=True)
    if not condition:
        raise AssertionError(name)


def agent_status():
    """Host-shell validation remains available without a browser session."""
    command = (f"sudo docker compose -p arknet-node --env-file /opt/arknet/.env "
               f"-f /opt/arknet/releases/{VERSION}/compose.yaml "
               "exec -T arknet-agent arknet-agent status --json")
    return json.loads(vm(command, check=True).stdout)


def redact_setup_codes(text):
    return re.sub(r"[A-Z2-7]{4}(?:-[A-Z2-7]{4}){7}", "[REDACTED setup code]", text)


def check_management(output):
    check("installer prints a one-time operator setup code",
          re.search(r"[A-Z2-7]{4}(?:-[A-Z2-7]{4}){7}", output) is not None)
    address = re.search(r"Management UI \(this network\): (https://([^/:]+):8443)", output)
    check("installer prints a LAN HTTPS management address",
          address is not None and address[2] not in ("127.0.0.1", "localhost", "0.0.0.0"))
    printed = re.search(r"Certificate SHA-256 fingerprint: ([0-9A-F:]{95})", output)
    actual = vm("sudo openssl x509 -in /opt/arknet/tls/cert.pem -noout -sha256 -fingerprint", check=True).stdout
    check("printed fingerprint matches the Node certificate", printed is not None and printed[1] in actual)
    response = vm("sudo curl --noproxy '*' --cacert /opt/arknet/tls/cert.pem -fsS -o /dev/null "
                  f"-w '%{{http_code}}' {address[1]}/", check=True).stdout
    check("LAN HTTPS serves the UI with the Node certificate", response == "200")
    published = json.loads(vm("sudo docker inspect $(sudo docker compose -p arknet-node "
                              f"--env-file /opt/arknet/.env -f /opt/arknet/releases/{VERSION}/compose.yaml "
                              "ps -q arknet-ui) --format '{{json .NetworkSettings.Ports}}'", check=True).stdout)
    http = published.get("5173/tcp") or []
    https = published.get("8443/tcp") or []
    check("plain HTTP is published only on loopback", bool(http) and all(p["HostIp"] in ("127.0.0.1", "::1") for p in http))
    check("HTTPS is published on LAN interfaces", any(p["HostIp"] in ("0.0.0.0", "::") for p in https))


def image(component):
    return f"{REGISTRY}/arknet/{component}:{VERSION}"


def publish_site(site: Path):
    """What an ArkNet release publisher produces."""
    release = site / "releases" / VERSION
    release.mkdir(parents=True)
    files = {}
    for name in ("compose.yaml", "compose.transfer.yaml"):
        text = (NODE_DIR / name).read_text()
        text = re.sub(r"(?m)^name: .*$", "name: arknet-node", text)
        text = re.sub(r"(?m)^    build:\n      context: .*\n", "", text)
        for component in ("arknet-agent", "arknet-ui", "arknet-transfer"):
            text = text.replace(f"image: {component}:0.0.1-dev", f"image: {image(component)}")
        files[name] = text.encode()
        (release / name).write_bytes(files[name])
    manifest = json.loads((NODE_DIR / "release.json").read_text())
    manifest.update(version=VERSION, channel="stable", published_at="2026-10-25T00:00:00Z")
    for component in ("arknet-agent", "arknet-ui", "arknet-transfer"):
        manifest["components"][component] = {"version": VERSION, "image": image(component), "bundled": True}
    manifest["compose"] = {"project": "arknet-node",
                           "files": [{"path": n, "sha256": hashlib.sha256(c).hexdigest()} for n, c in files.items()]}
    data = (json.dumps(manifest, indent=2) + "\n").encode()
    (release / "release.json").write_bytes(data)
    (site / "index.json").write_text(json.dumps({"schema": "arknet.node-release-index/v1", "channels": {"stable": {
        "version": VERSION, "manifest_url": f"releases/{VERSION}/release.json", "manifest_sha256": hashlib.sha256(data).hexdigest()}}}))
    shutil.copy(NODE_DIR / "arknet-node", site / "arknet-node")
    installer = (NODE_DIR / "install.sh").read_text().replace("__ARKNET_RELEASE_URL__", f"http://{GATEWAY}:{SITE_PORT}")
    (site / "install.sh").write_text(installer)


def main():
    keep_vm = "--keep-vm" in sys.argv
    work = Path(tempfile.mkdtemp(prefix="arknet-install-check-", dir=os.environ.get("TMPDIR")))
    site = work / "site"
    site.mkdir()
    procs, server = [], None
    pg_password = secrets.token_hex(24)
    try:
        # ---- Core
        sh("docker", "run", "-d", "--name", PG_NAME, "-e", f"POSTGRES_PASSWORD={pg_password}", "-e", "POSTGRES_USER=arknet",
           "-e", "POSTGRES_DB=core", "-p", f"127.0.0.1:{PG_PORT}:5432", "postgres:18-alpine")
        for _ in range(60):
            if sh("docker", "exec", PG_NAME, "pg_isready", "-U", "arknet", check=False).returncode == 0:
                break
            time.sleep(1)
        time.sleep(2)
        core_bin = work / "arknet-core"
        sh("go", "build", "-o", str(core_bin), ".", cwd=ROOT / "core")
        core_log = open(work / "core.log", "w")
        core_env = {"PATH": os.environ["PATH"], "ARKNET_CORE_ADDR": f"127.0.0.1:{CORE_PORT}",
                    "DATABASE_URL": f"postgres://arknet:{pg_password}@127.0.0.1:{PG_PORT}/core?sslmode=disable"}
        procs.append(subprocess.Popen([str(core_bin)], stdout=core_log, stderr=subprocess.STDOUT, env=core_env))
        for _ in range(30):
            try:
                urllib.request.urlopen(f"http://127.0.0.1:{CORE_PORT}/health", timeout=2)
                break
            except OSError:
                time.sleep(1)

        # Scoped administrator token from the shell CLI (ARKSEC-005).
        admin_token = json.loads(subprocess.run(
            [str(core_bin), "admin", "create-token", "--name", "install-vm-check", "--permission", "nodes.decide", "--ttl", "2h"],
            env=core_env, check=True, capture_output=True, text=True).stdout)["token"]

        def pending():
            request = urllib.request.Request(f"http://127.0.0.1:{CORE_PORT}/api/admin/v1/nodes/pending",
                                             headers={"Authorization": f"Bearer {admin_token}"})
            with urllib.request.urlopen(request, timeout=5) as response:
                return [n["node_id"] for n in json.loads(response.read())["nodes"]]

        # ---- Images and registry
        sh("docker", "run", "-d", "--name", REGISTRY_NAME, "-p", f"127.0.0.1:{REGISTRY_PORT}:5000", "registry:latest")
        sh("docker", "build", "-q", "--build-arg", f"ARKNET_VERSION={VERSION}", "-t", image("arknet-agent"), str(ROOT / "node-agent"))
        sh("docker", "build", "-q", "-t", image("arknet-ui"), str(ROOT / "node-ui"))
        sh("docker", "tag", "arknet-transfer:0.0.1-dev", image("arknet-transfer"))
        for component in ("arknet-agent", "arknet-ui", "arknet-transfer"):
            for _ in range(10):
                if sh("docker", "push", "-q", image(component), check=False).returncode == 0:
                    break
                time.sleep(1)
            else:
                raise RuntimeError("registry push failed")

        # ---- Release site
        publish_site(site)

        class Quiet(http.server.SimpleHTTPRequestHandler):
            def log_message(self, *args):
                pass
        server = http.server.ThreadingHTTPServer(("127.0.0.1", SITE_PORT), functools.partial(Quiet, directory=str(site)))
        threading.Thread(target=server.serve_forever, daemon=True).start()

        # ---- Fresh VM
        sh("limactl", "delete", "-f", VM, check=False)
        sh("limactl", "create", "--name", VM, "--tty=false", "--cpus", "2", "--memory", "4", "--disk", "30",
           "--set", ".mounts=[] | .containerd.system=false | .containerd.user=false", "template://ubuntu-24.04", timeout=900)
        sh("limactl", "start", VM, "--tty=false", timeout=900)
        fresh = vm(". /etc/os-release; echo $PRETTY_NAME; command -v docker || echo no-docker; test -e /opt/arknet || echo no-arknet").stdout
        check("VM is a fresh Ubuntu LTS without Docker or ArkNet", "Ubuntu 24.04" in fresh and "no-docker" in fresh and "no-arknet" in fresh,
              fresh.strip().replace("\n", ", "))
        ssh_config = Path.home() / ".lima" / VM / "ssh.config"
        procs.append(subprocess.Popen(["ssh", "-F", str(ssh_config), "-N", "-o", "ExitOnForwardFailure=yes",
                                       "-R", f"{REGISTRY_PORT}:127.0.0.1:{REGISTRY_PORT}", f"lima-{VM}"],
                                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        time.sleep(3)

        # ---- Negative checks on the real VM
        out = vm(f"curl -fsSLO http://{GATEWAY}:{SITE_PORT}/install.sh && sh install.sh --core-url http://{GATEWAY}:{CORE_PORT} --allow-insecure-http")
        check("non-root run gives an actionable error", out.returncode != 0 and "must run as root" in out.stderr and "sudo sh install.sh" in out.stderr)
        out = vm("sudo mkdir -p /mnt/small && sudo mount -t tmpfs -o size=1g tmpfs /mnt/small && "
                 f"sudo sh install.sh --home /mnt/small/arknet --core-url http://{GATEWAY}:{CORE_PORT} --allow-insecure-http; "
                 "rc=$?; sudo umount /mnt/small; exit $rc")
        check("too little disk gives an actionable error", out.returncode != 0 and "Not enough free disk space on /mnt/small" in out.stderr,
              out.stderr.strip().splitlines()[0] if out.stderr.strip() else "")
        check("failed pre-flight leaves the VM unchanged", "no-docker" in vm("command -v docker || echo no-docker").stdout)

        # ---- ACCEPTANCE: the documented installation process
        started = time.monotonic()
        out = vm(f"curl -fsSLO http://{GATEWAY}:{SITE_PORT}/install.sh && "
                 f"sudo sh install.sh --core-url http://{GATEWAY}:{CORE_PORT} --allow-insecure-http")
        minutes = (time.monotonic() - started) / 60
        print("\n".join("    | " + line for line in redact_setup_codes(out.stdout + out.stderr).splitlines()), flush=True)
        check_management(out.stdout)
        node_id = (re.search(r"Node ID: (node_[0-9a-f]+)", out.stdout) or [None, None])[1]
        check("installer completed", out.returncode == 0 and "INSTALLED: ArkNet 0.1.0 (stable)" in out.stdout, f"exit {out.returncode}, {minutes:.1f} min")
        check("ACCEPTANCE fresh VM reaches ENROLLMENT_PENDING", "Node state: ENROLLMENT_PENDING" in out.stdout and node_id is not None, node_id)
        check("Core lists the Node as pending enrollment", node_id in pending())
        status = agent_status()
        check("Agent reports the installed release and healthy services",
              status["release"] == {"version": VERSION, "channel": "stable"} and status["jellyfin_healthy"] is True
              and status["state"] == "ENROLLMENT_PENDING")
        check("setup UI is served on the VM's loopback", vm("curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:5174/").stdout == "200")
        perms = vm("sudo stat -c '%a %n' /opt/arknet /opt/arknet/.env /opt/arknet/backups /opt/arknet/releases/0.1.0/release.json").stdout
        check("installation directories and secrets have safe permissions",
              "755 /opt/arknet\n" in perms and "600 /opt/arknet/.env" in perms and "700 /opt/arknet/backups" in perms and "644 " in perms, perms.strip())
        check("operator tool installed", "ArkNet 0.1.0 (stable channel)" in vm("sudo arknet-node status").stdout)
        env_hash = vm("sudo sha256sum /opt/arknet/.env").stdout

        # ---- Re-run safety
        out = vm(f"sudo sh install.sh --core-url http://{GATEWAY}:{CORE_PORT} --allow-insecure-http")
        status = agent_status()
        check("re-running the installer is safe", out.returncode == 0 and "already installed" in out.stdout
              and vm("sudo sha256sum /opt/arknet/.env").stdout == env_hash and status["node_id"] == node_id
              and status["state"] == "ENROLLMENT_PENDING" and pending().count(node_id) == 1)
        log = vm("sudo cat /opt/arknet/upgrade.log").stdout
        secret = vm("sudo sed -n 's/^POSTGRES_PASSWORD=//p' /opt/arknet/.env").stdout.strip()
        check("no secrets in installer output or logs", secret and secret not in log and secret not in out.stdout + out.stderr)
    except Exception:
        log_path = work / "core.log"
        if log_path.exists():
            print("Core log tail:\n" + "\n".join(log_path.read_text().splitlines()[-15:]))
        raise
    finally:
        for proc in procs:
            proc.terminate()
        for proc in procs:
            try:
                proc.wait(10)
            except subprocess.TimeoutExpired:
                proc.kill()
        if server:
            server.shutdown()
            server.server_close()
        if not keep_vm:
            sh("limactl", "delete", "-f", VM, check=False)
        sh("docker", "rm", "-f", "-v", PG_NAME, REGISTRY_NAME, check=False)
        for component in ("arknet-agent", "arknet-ui", "arknet-transfer"):
            sh("docker", "rmi", image(component), check=False)
        shutil.rmtree(work, ignore_errors=True)
    ok = bool(results) and all(results.values())
    print("ALL PASS" if ok else "INCOMPLETE")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
