#!/usr/bin/env python3
"""Live check: upgrade a test Node between ArkNet development releases.

Builds two version-stamped Agent images, publishes development releases to a
throwaway local registry and loopback release host, installs release A, then
uses only `deploy/node/arknet-node` to:

  1. upgrade A -> B (acceptance: data and volumes preserved, Agent + Jellyfin validated);
  2. refuse a release whose images cannot be pulled (nothing changed);
  3. roll back automatically from a release that fails after apply;
  4. roll back manually to A.

Everything created is removed at the end. Requires Docker with Compose v2.
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
TOOL = NODE_DIR / "arknet-node"
PROJECT = "arknet-upgrade-test"
REGISTRY_NAME = "arknet-upgrade-test-registry"
REGISTRY = "localhost:15039"
AGENT_PORT, UI_PORT, JELLYFIN_PORT = 18110, 18111, 18112
A, B, BROKEN, UNPULLABLE = "0.0.2-dev.1", "0.0.2-dev.2", "0.0.2-dev.3", "0.0.2-dev.4"
results = {}


def sh(*args, check=True, **kw):
    return subprocess.run(list(args), check=check, capture_output=True, text=True, **kw)


def check(name, condition, detail=""):
    results[name] = bool(condition)
    print(("PASS " if condition else "FAIL ") + name + (f" ({detail})" if detail else ""), flush=True)
    if not condition:
        raise AssertionError(name)


def agent(path, method="GET", body=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(f"http://127.0.0.1:{AGENT_PORT}{path}", data=data, method=method)
    if data is not None:
        request.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.loads(response.read())


def image(name, version):
    return f"{REGISTRY}/arknet/{name}:{version}"


def release_files(version, broken=False):
    """Release Compose files: registry images, no build sections, fixed project."""
    out = {}
    for name in ("compose.yaml", "compose.transfer.yaml"):
        text = (NODE_DIR / name).read_text()
        text = re.sub(r"(?m)^name: .*$", f"name: {PROJECT}", text)
        text = re.sub(r"(?m)^    build:\n      context: .*\n", "", text)
        for component in ("arknet-agent", "arknet-ui", "arknet-transfer"):
            text = text.replace(f"image: {component}:0.0.1-dev", f"image: {image(component, version)}")
        if broken and name == "compose.yaml":  # an invalid Agent setting: the container exits and never becomes healthy
            text = text.replace("ARKNET_HEARTBEAT_INTERVAL: ${ARKNET_HEARTBEAT_INTERVAL:-30s}", "ARKNET_HEARTBEAT_INTERVAL: 1s")
            assert "ARKNET_HEARTBEAT_INTERVAL: 1s" in text
        out[name] = text.encode()
    manifest = json.loads((NODE_DIR / "release.json").read_text())
    manifest["version"] = version
    manifest["published_at"] = "2026-10-25T00:00:00Z"
    for component in ("arknet-agent", "arknet-ui", "arknet-transfer"):
        manifest["components"][component] = {"version": version, "image": image(component, version), "bundled": True}
    manifest["compose"] = {"project": PROJECT, "files": [
        {"path": n, "sha256": hashlib.sha256(c).hexdigest()} for n, c in out.items()]}
    out["release.json"] = (json.dumps(manifest, indent=2) + "\n").encode()
    return out


def main():
    # Docker bind-mounts release files from here. macOS TMPDIR (/var/folders/...)
    # is not reliably visible to Docker Desktop: a file mount silently becomes an
    # empty directory and the Agent reports release_manifest_invalid. The
    # checkout is always shared; /.arknet-* is git-ignored and removed below.
    work = Path(tempfile.mkdtemp(prefix=".arknet-upgrade-check-", dir=ROOT))
    home, site = work / "home", work / "site"
    home.mkdir()
    home.chmod(0o755)  # as the installer creates it: the Agent container reads releases/*/release.json
    site.mkdir()
    server = None
    env_file = home / ".env"
    pushed = []
    try:
        # Registry and images.
        sh("docker", "run", "-d", "--name", REGISTRY_NAME, "-p", f"127.0.0.1:{REGISTRY.split(':')[1]}:5000", "registry:latest")
        for version, build in ((A, True), (B, True), (BROKEN, False)):
            agent_ref = image("arknet-agent", version)
            if build:
                sh("docker", "build", "-q", "--build-arg", f"ARKNET_VERSION={version}", "-t", agent_ref, str(ROOT / "node-agent"))
            else:
                sh("docker", "tag", image("arknet-agent", B), agent_ref)
            sh("docker", "tag", "arknet-ui:0.0.1-dev", image("arknet-ui", version))
            sh("docker", "tag", "arknet-transfer:0.0.1-dev", image("arknet-transfer", version))
            for component in ("arknet-agent", "arknet-ui", "arknet-transfer"):
                ref = image(component, version)
                for attempt in range(10):
                    if sh("docker", "push", "-q", ref, check=False).returncode == 0:
                        break
                    time.sleep(1)
                else:
                    raise RuntimeError(f"push failed: {ref}")
                pushed.append(ref)
        # Only registry copies remain, so every install/upgrade really pulls.
        for ref in pushed:
            sh("docker", "rmi", ref)

        # Release host.
        for version, broken in ((A, False), (B, False), (BROKEN, True), (UNPULLABLE, False)):
            directory = site / "releases" / version
            directory.mkdir(parents=True)
            for name, content in release_files(version, broken).items():
                (directory / name).write_bytes(content)
        class Quiet(http.server.SimpleHTTPRequestHandler):
            def log_message(self, *args):
                pass
        handler = functools.partial(Quiet, directory=str(site))
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        index_url = f"http://127.0.0.1:{server.server_address[1]}/index.json"
        # The Agent container reaches the same host-loopback release host via
        # Docker Desktop's host.docker.internal (Prompt 38 discovery).
        agent_index_url = f"http://host.docker.internal:{server.server_address[1]}/index.json"

        def publish(version):
            digest = hashlib.sha256((site / "releases" / version / "release.json").read_bytes()).hexdigest()
            (site / "index.json").write_text(json.dumps({"schema": "arknet.node-release-index/v1", "channels": {
                "development": {"version": version, "manifest_url": f"releases/{version}/release.json", "manifest_sha256": digest}}}))

        # Install release A (what the Prompt 40 installer will do).
        old = os.umask(0o077)
        env_file.write_text(f"POSTGRES_PASSWORD={secrets.token_hex(24)}\nARKNET_AGENT_PORT={AGENT_PORT}\n"
                            f"ARKNET_UI_PORT={UI_PORT}\nJELLYFIN_BIND=127.0.0.1:{JELLYFIN_PORT}\n"
                            f"ARKNET_RELEASE_INDEX_URL={agent_index_url}\nARKNET_RELEASE_INDEX_ALLOW_HTTP=true\n")
        os.umask(old)
        shutil.copytree(site / "releases" / A, home / "releases" / A)
        probe = subprocess.run(["docker", "run", "--rm", "--network", "none", "--entrypoint", "sh", "-v",
                                f"{home / 'releases' / A / 'release.json'}:/probe:ro", "postgres:18-alpine",
                                "-c", "test -f /probe && test -s /probe"], capture_output=True, timeout=120)
        check("Docker can see the release bind mounts", probe.returncode == 0,
              "" if probe.returncode == 0 else "run from a Docker-shared path such as the repository checkout")
        (home / "state.json").write_text(json.dumps({"schema": "arknet.node-install/v1", "release": A, "previous": None,
                                                    "project": PROJECT, "compose_files": ["compose.yaml"]}))
        for component in ("arknet-agent", "arknet-ui"):
            sh("docker", "pull", "-q", image(component, A))
        sh("docker", "compose", "-p", PROJECT, "--env-file", str(env_file), "-f", str(home / "releases" / A / "compose.yaml"),
           "up", "-d", "--wait", "--wait-timeout", "300")
        status = agent("/api/v1/status")
        check("release A installed and running", status["release"] == {"version": A, "channel": "development"}, status["release"])

        # Data that must survive.
        node_id = status["node_id"]
        agent("/api/v1/settings/automatic-updates", "PUT", {"enabled": False})
        agent("/api/v1/settings/media-miss-policy", "PUT", {"policy": "MANUAL"})
        sh("docker", "compose", "-p", PROJECT, "--env-file", str(env_file), "-f", str(home / "releases" / A / "compose.yaml"),
           "exec", "-T", "jellyfin", "sh", "-c", "echo kept > /config/arknet-upgrade-marker")
        volumes = lambda: {n: sh("docker", "volume", "inspect", "-f", "{{.CreatedAt}}", n).stdout.strip()
                           for n in sh("docker", "volume", "ls", "-q", "--filter", f"label=com.docker.compose.project={PROJECT}").stdout.split()}
        volumes_before = volumes()

        def tool(*args):
            started = time.monotonic()
            result = sh(sys.executable, "-I", str(TOOL), "--home", str(home), *args, check=False)
            print("\n".join("    | " + l for l in result.stdout.splitlines()), flush=True)
            return result.returncode, result.stdout, time.monotonic() - started

        def preserved(release):
            status = agent("/api/v1/status")
            marker = sh("docker", "compose", "-p", PROJECT, "--env-file", str(env_file), "-f",
                        str(home / "releases" / release / "compose.yaml"), "exec", "-T", "jellyfin", "cat", "/config/arknet-upgrade-marker",
                        check=False).stdout.strip()
            return (status["node_id"] == node_id and agent("/api/v1/settings/automatic-updates") == {"enabled": False}
                    and agent("/api/v1/settings/media-miss-policy") == {"policy": "MANUAL"} and marker == "kept"
                    and volumes() == volumes_before and status["jellyfin_healthy"] is True)

        # 1. Update discovery (Prompt 38): the running Agent finds B but installs nothing.
        publish(B)
        discovered = agent("/api/v1/updates/check", "POST", {})
        check("Agent update discovery finds B while A keeps running",
              discovered["discovery"]["state"] == "UPDATE_AVAILABLE" and discovered["current_version"] == A
              and (discovered["available"] or {}).get("version") == B
              and agent("/api/v1/status")["release"]["version"] == A,
              f"state {discovered['discovery']['state']}, current {discovered['current_version']}, "
              f"available {(discovered['available'] or {}).get('version')}")

        # 2. Acceptance: A -> B with only arknet-node.
        absent = sh("docker", "image", "inspect", image("arknet-agent", B), check=False).returncode != 0
        code, out, seconds = tool("upgrade", "--index", index_url)
        status = agent("/api/v1/status")
        check("ACCEPTANCE upgrade A -> B succeeded via arknet-node", code == 0 and "UPGRADED" in out and status["release"]["version"] == B,
              f"exit {code}, {seconds:.0f}s, Agent reports {status['release']}")
        check("required images were pulled from the registry",
              absent and sh("docker", "image", "inspect", image("arknet-agent", B), check=False).returncode == 0)
        check("identity, settings, Jellyfin data and volumes preserved", preserved(B))
        state = json.loads((home / "state.json").read_text())
        backups = list((home / "backups").iterdir())
        check("state records B with A as previous; pre-upgrade backup kept",
              state["release"] == B and state["previous"] == A and len(backups) == 1 and backups[0].stat().st_size > 0
              and backups[0].stat().st_mode & 0o777 == 0o600)
        code, out, _ = tool("upgrade", "--index", index_url)
        check("re-running is a no-op when up to date", code == 0 and "Nothing to do" in out)

        # 2. Unpullable release: aborts before changing anything.
        publish(UNPULLABLE)
        code, out, _ = tool("upgrade", "--index", index_url)
        check("unpullable release aborts with no change", code == 2 and "FAILED at images" in out and "not changed" in out
              and agent("/api/v1/status")["release"]["version"] == B)

        # 3. Release that fails after apply: automatic rollback.
        publish(BROKEN)
        code, out, _ = tool("upgrade", "--index", index_url)
        status = agent("/api/v1/status")
        check("failed release is reported and rolled back automatically",
              code == 3 and "UPGRADE FAILED while applying" in out and "ROLLED BACK" in out and status["release"]["version"] == B,
              f"exit {code}")
        check("data preserved through failed upgrade and rollback", preserved(B))

        # 4. Operator rollback to A.
        code, out, _ = tool("rollback")
        status = agent("/api/v1/status")
        check("manual rollback returns to A", code == 0 and status["release"]["version"] == A)
        check("data preserved through rollback", preserved(A))
        log = (home / "upgrade.log").read_text()
        password = env_file.read_text().split("POSTGRES_PASSWORD=")[1].split("\n")[0]
        check("upgrade log contains no secrets", password not in log)
    except Exception:
        logs = sh("docker", "logs", f"{PROJECT}-arknet-agent-1", check=False)
        print("Agent log (release/startup lines):")
        for line in (logs.stdout + logs.stderr).splitlines():
            if "release_manifest" in line or '"level":"ERROR"' in line:
                print("    " + line)
        raise
    finally:
        if (home / "state.json").exists() or env_file.exists():
            for release in sorted((home / "releases").glob("*")) if (home / "releases").exists() else []:
                sh("docker", "compose", "-p", PROJECT, "--env-file", str(env_file), "-f", str(release / "compose.yaml"),
                   "down", "-v", "--remove-orphans", check=False)
                break
        if server:
            server.shutdown()
            server.server_close()
        sh("docker", "rm", "-f", "-v", REGISTRY_NAME, check=False)
        for version in (A, B, BROKEN):
            for component in ("arknet-agent", "arknet-ui", "arknet-transfer"):
                sh("docker", "rmi", image(component, version), check=False)
        shutil.rmtree(work, ignore_errors=True)
    print("ALL PASS" if results and all(results.values()) else "INCOMPLETE")
    return 0 if results and all(results.values()) else 1


if __name__ == "__main__":
    sys.exit(main())
