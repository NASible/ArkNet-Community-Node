#!/usr/bin/env python3
"""Live check: upgrade a test Node between ArkNet development releases.

Builds two version-stamped Agent images, publishes development releases to a
throwaway local registry and loopback release host, installs release A, then
uses only `deploy/node/arknet-node` to:

  0. refuse a tampered or unsigned B, and keep the Agent from reporting it (nothing changed);
  1. upgrade A -> B (acceptance: data and volumes preserved, Agent + Jellyfin validated);
  2. refuse a release whose images cannot be pulled (nothing changed);
  3. roll back automatically from a release that fails after apply;
  4. roll back manually to A.

Management routes need an operator (Prompt 45B). The check signs in at A,
disables that operator, and expects `arknet-node upgrade` to print a one-time
setup code plus the HTTPS certificate fingerprint, which it then uses.

Releases are produced by the real release-site generator (scripts/release_site.py):
digest-pinned images and Ed25519-signed manifests, with a throwaway signing key
generated for this run (Prompt 51A).

Everything created is removed at the end. Requires Docker with Compose v2 and OpenSSL 3.
"""
import functools
import hashlib
import http.server
import json
import os
import re
import secrets
import shutil
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from scripts import operator_login, release_site  # noqa: E402

ROOT = Path(__file__).resolve().parents[1]
NODE_DIR = ROOT / "deploy" / "node"
TOOL = NODE_DIR / "arknet-node"
PROJECT = "arknet-upgrade-test"
REGISTRY_NAME = "arknet-upgrade-test-registry"
REGISTRY = "localhost:15039"
AGENT_PORT, UI_PORT, JELLYFIN_PORT, HTTPS_PORT, MEMBER_PORT = 18110, 18111, 18112, 18113, 18114
UI_SOURCE_IMAGE = "arknet-ui:upgrade-test-source"
session = {}  # operator cookie header for agent()
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
    request = urllib.request.Request(f"http://127.0.0.1:{AGENT_PORT}{path}", data=data, method=method, headers=dict(session))
    if data is not None:
        request.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.loads(response.read())


def raw_request(url, method="GET", body=None, headers=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(url, data=data, method=method, headers={"Content-Type": "application/json", **(headers or {})})
    try:
        with urllib.request.urlopen(request, timeout=20) as response:
            return response.status, dict(response.headers), response.read()
    except urllib.error.HTTPError as error:
        return error.code, dict(error.headers), error.read()


def served_fingerprint(port):
    """SHA-256 of the certificate the HTTPS listener presents, browser style."""
    import socket
    context = ssl._create_unverified_context()  # compare the self-signed certificate explicitly below
    with socket.create_connection(("127.0.0.1", port), timeout=10) as connection:
        with context.wrap_socket(connection, server_hostname="localhost") as secured:
            digest = hashlib.sha256(secured.getpeercert(binary_form=True)).hexdigest().upper()
    return ":".join(digest[i:i + 2] for i in range(0, 64, 2))


def image(name, version):
    return f"{REGISTRY}/arknet/{name}:{version}"


digests = {}  # tag reference -> pushed registry digest


def pinned(name, version):
    """The digest-pinned reference a published release uses."""
    ref = image(name, version)
    return f"{ref}@{digests[ref]}"


def pushed_digest(ref):
    repo = ref.rsplit(":", 1)[0]
    for entry in json.loads(sh("docker", "image", "inspect", "--format", "{{json .RepoDigests}}", ref).stdout):
        if entry.startswith(repo + "@sha256:"):
            return entry.split("@", 1)[1]
    raise RuntimeError(f"no registry digest for {ref}")


def release_files(version, broken=False):
    """A signed-release payload from the real generator: digest-pinned images, fixed project."""
    images = {c: pinned(c, version) for c in release_site.NODE_COMPONENTS}
    out = release_site.node_release(version, images, PROJECT, "2026-10-25T00:00:00Z", NODE_DIR)
    if broken:  # an invalid Agent setting: the container exits and never becomes healthy
        text = out["compose.yaml"].decode()
        text = text.replace("ARKNET_HEARTBEAT_INTERVAL: ${ARKNET_HEARTBEAT_INTERVAL:-30s}", "ARKNET_HEARTBEAT_INTERVAL: 1s")
        assert "ARKNET_HEARTBEAT_INTERVAL: 1s" in text
        out["compose.yaml"] = text.encode()
        manifest = json.loads(out["release.json"])
        for item in manifest["compose"]["files"]:
            item["sha256"] = hashlib.sha256(out[item["path"]]).hexdigest()
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
    openssl = release_site.openssl3()
    signing_key = work / "release-signing.pem"  # throwaway, removed with the work directory
    try:
        sh(openssl, "genpkey", "-algorithm", "ed25519", "-out", str(signing_key))
        signing_key.chmod(0o600)
        verify_key = release_site.public_key(signing_key, openssl)
        domain = release_site.load_arknet_node(NODE_DIR).SIGNATURE_DOMAIN
        # Registry and images.
        sh("docker", "run", "-d", "--name", REGISTRY_NAME, "-p", f"127.0.0.1:{REGISTRY.split(':')[1]}:5000", "registry:latest")
        # The UI image under test (HTTPS listener) is built from this checkout.
        sh("docker", "build", "-q", "-t", UI_SOURCE_IMAGE, str(ROOT / "node-ui"))
        for version, build in ((A, True), (B, True), (BROKEN, False)):
            agent_ref = image("arknet-agent", version)
            if build:
                sh("docker", "build", "-q", "--build-arg", f"ARKNET_VERSION={version}", "-t", agent_ref, str(ROOT / "node-agent"))
            else:
                sh("docker", "tag", image("arknet-agent", B), agent_ref)
            sh("docker", "tag", UI_SOURCE_IMAGE, image("arknet-ui", version))
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
                digests[ref] = pushed_digest(ref)
        # A release whose images were never pushed.
        for component in release_site.NODE_COMPONENTS:
            digests[image(component, UNPULLABLE)] = "sha256:" + hashlib.sha256(component.encode()).hexdigest()
        # Only registry copies remain, so every install/upgrade really pulls.
        for ref in pushed:
            sh("docker", "rmi", ref)

        # Release host.
        for version, broken in ((A, False), (B, False), (BROKEN, True), (UNPULLABLE, False)):
            directory = site / "releases" / version
            directory.mkdir(parents=True)
            for name, content in release_files(version, broken).items():
                (directory / name).write_bytes(content)
            data = (directory / "release.json").read_bytes()
            (directory / "release.json.sig").write_bytes(release_site.sign(data, signing_key, domain, openssl))
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
                            f"ARKNET_MEMBER_PORT={MEMBER_PORT}\nARKNET_UI_PORT={UI_PORT}\nJELLYFIN_BIND=127.0.0.1:{JELLYFIN_PORT}\n"
                            f"ARKNET_UI_HTTPS_BIND=127.0.0.1:{HTTPS_PORT}\n"
                            f"ARKNET_RELEASE_INDEX_URL={agent_index_url}\nARKNET_RELEASE_INDEX_ALLOW_HTTP=true\n"
                            f"ARKNET_RELEASE_VERIFY_KEY={verify_key}\n")
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
            sh("docker", "pull", "-q", pinned(component, A))
        sh("docker", "compose", "-p", PROJECT, "--env-file", str(env_file), "-f", str(home / "releases" / A / "compose.yaml"),
           "up", "-d", "--wait", "--wait-timeout", "300")
        compose_a = ["docker", "compose", "-p", PROJECT, "--env-file", str(env_file), "-f", str(home / "releases" / A / "compose.yaml")]
        status, _, _ = raw_request(f"http://127.0.0.1:{AGENT_PORT}/api/v1/status")
        check("management API refuses an anonymous caller", status == 401, f"HTTP {status}")
        code = operator_login.setup_code(sh(*compose_a, "exec", "-T", "arknet-agent", "arknet-agent", "operator", "setup-code").stdout)
        first = operator_login.bootstrap(f"http://127.0.0.1:{AGENT_PORT}", code, raw_request, username="operator-a",
                                         member_base=f"http://127.0.0.1:{MEMBER_PORT}")
        session.update(first.headers)
        status = agent("/api/v1/status")
        check("release A installed and running", status["release"] == {"version": A, "channel": "development"}, status["release"])
        member_status = raw_request(f"http://127.0.0.1:{MEMBER_PORT}/api/v1/status")[0]
        management_login = raw_request(f"http://127.0.0.1:{AGENT_PORT}/api/v1/global-users/sessions", "POST", {})[0]
        check("member and management listeners are separated", member_status == 404 and management_login == 404,
              f"member status {member_status}, management member-login {management_login}")

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
            result = sh(sys.executable, "-I", str(TOOL), "--home", str(home), *args, check=False,
                        env=release_site.with_openssl_on_path(openssl))
            # Setup codes are secrets shown once: never echo them into the check's own output.
            print("\n".join("    | " + l for l in operator_login.redact_codes(result.stdout).splitlines()), flush=True)
            return result.returncode, result.stdout, time.monotonic() - started

        def preserved(release):
            status = agent("/api/v1/status")
            marker = sh("docker", "compose", "-p", PROJECT, "--env-file", str(env_file), "-f",
                        str(home / "releases" / release / "compose.yaml"), "exec", "-T", "jellyfin", "cat", "/config/arknet-upgrade-marker",
                        check=False).stdout.strip()
            return (status["node_id"] == node_id and agent("/api/v1/settings/automatic-updates") == {"enabled": False}
                    and agent("/api/v1/settings/media-miss-policy") == {"policy": "MANUAL"} and marker == "kept"
                    and volumes() == volumes_before and status["jellyfin_healthy"] is True)

        # 0. Signed releases (Prompt 51A): a tampered or unsigned B is refused with no change.
        genuine = {name: (site / "releases" / B / name).read_bytes() for name in ("release.json", "release.json.sig")}
        manifest_b = json.loads(genuine["release.json"])
        manifest_b["components"]["arknet-agent"]["image"] = f"{REGISTRY}/evil/arknet-agent:{B}@sha256:" + "e" * 64
        (site / "releases" / B / "release.json").write_bytes((json.dumps(manifest_b, indent=2) + "\n").encode())
        publish(B)  # the attacker controls the release host: the index digest matches the tampered manifest
        first_check = time.monotonic()
        discovered = agent("/api/v1/updates/check", "POST", {})
        check("Agent update discovery refuses a tampered B",
              discovered["discovery"]["state"] == "CHECK_FAILED" and discovered["available"] is None,
              f"state {discovered['discovery']['state']}")

        def refused(label):
            code, out, _ = tool("upgrade", "--index", index_url)
            check(f"{label} B is refused with no change",
                  code == 2 and "FAILED at download" in out and "signature" in out and "not changed" in out
                  and not (home / "releases" / B).exists() and agent_version() == A
                  and sh("docker", "image", "inspect", pinned("arknet-agent", B), check=False).returncode != 0,
                  f"exit {code}")
        agent_version = lambda: json.loads(sh(*compose_a, "exec", "-T", "arknet-agent", "arknet-agent", "status", "--json").stdout)["release"]["version"]
        refused("tampered")
        (site / "releases" / B / "release.json").write_bytes(genuine["release.json"])
        (site / "releases" / B / "release.json.sig").unlink()
        publish(B)
        refused("unsigned")
        (site / "releases" / B / "release.json.sig").write_bytes(genuine["release.json.sig"])
        time.sleep(max(0.0, 11 - (time.monotonic() - first_check)))  # manual checks coalesce for 10 s

        # 1. Update discovery (Prompt 38): the running Agent finds B but installs nothing.
        publish(B)
        discovered = agent("/api/v1/updates/check", "POST", {})
        check("Agent update discovery finds B while A keeps running",
              discovered["discovery"]["state"] == "UPDATE_AVAILABLE" and discovered["current_version"] == A
              and (discovered["available"] or {}).get("version") == B
              and agent("/api/v1/status")["release"]["version"] == A,
              f"state {discovered['discovery']['state']}, current {discovered['current_version']}, "
              f"available {(discovered['available'] or {}).get('version')}")

        # Leave the Node without an active operator, as an existing pre-45B Node is.
        sh(*compose_a, "exec", "-T", "arknet-agent", "arknet-agent", "operator", "disable", "operator-a")
        session.clear()
        check("disabled operator's session no longer works",
              raw_request(f"http://127.0.0.1:{AGENT_PORT}/api/v1/status", headers=first.headers)[0] == 401)

        # 2. Acceptance: A -> B with only arknet-node.
        absent = sh("docker", "image", "inspect", pinned("arknet-agent", B), check=False).returncode != 0
        code, out, seconds = tool("upgrade", "--index", index_url)
        printed = re.search(r"Certificate SHA-256 fingerprint: ([0-9A-F:]{95})", out)
        check("upgrade printed the management HTTPS address and certificate fingerprint",
              printed is not None and f"https://" in out and printed.group(1) == served_fingerprint(HTTPS_PORT),
              "printed fingerprint matches the certificate served on the HTTPS listener" if printed else "not printed")
        upgrade_code = operator_login.setup_code(out)
        second = operator_login.bootstrap(f"http://127.0.0.1:{AGENT_PORT}", upgrade_code, raw_request, username="operator-b",
                                          member_base=f"http://127.0.0.1:{MEMBER_PORT}")
        session.update(second.headers)
        check("setup code printed by the upgrade creates an operator", True)
        status = agent("/api/v1/status")
        check("ACCEPTANCE upgrade A -> B succeeded via arknet-node", code == 0 and "UPGRADED" in out and status["release"]["version"] == B,
              f"exit {code}, {seconds:.0f}s, Agent reports {status['release']}")
        check("required images were pulled from the registry by digest",
              absent and sh("docker", "image", "inspect", pinned("arknet-agent", B), check=False).returncode == 0)
        check("B was installed only after its signature verified",
              "signature verified (release key SHA256:" in out and (home / "releases" / B / "release.json.sig").exists())
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
        check("upgrade log contains no secrets", password not in log and upgrade_code not in log
              and upgrade_code.replace("-", "") not in log and "PRIVATE KEY" not in log)
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
        sh("docker", "rmi", UI_SOURCE_IMAGE, check=False)
        # Tagged and digest-pulled copies alike. Remove references, never image
        # IDs: an ID would also delete shared tags such as arknet-transfer:0.0.1-dev.
        listing = sh("docker", "image", "ls", "--digests", "--format", "{{.Repository}} {{.Tag}} {{.Digest}}",
                     "--filter", f"reference={REGISTRY}/arknet/*", check=False).stdout.splitlines()
        refs = set()
        for line in listing:
            repo, tag, digest = (line.split() + ["", "", ""])[:3]
            if tag and tag != "<none>":
                refs.add(f"{repo}:{tag}")
            if digest.startswith("sha256:"):
                refs.add(f"{repo}@{digest}")
        if refs:
            sh("docker", "rmi", *sorted(refs), check=False)
        shutil.rmtree(work, ignore_errors=True)
    print("ALL PASS" if results and all(results.values()) else "INCOMPLETE")
    return 0 if results and all(results.values()) else 1


if __name__ == "__main__":
    sys.exit(main())
