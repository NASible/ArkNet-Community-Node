"""Build the files of an ArkNet Node release site (Prompt 51A).

Used by `scripts/publish-release.py` for real releases and by the upgrade and
fresh-VM checks with throwaway registries and keys, so every check exercises
the same generator. Pure functions plus OpenSSL 3 for Ed25519 signing.

Release site layout (served as static files, for example GitHub Pages):

    index.json                      channel -> newest version, manifest URL, SHA-256
    install.sh                      release URL and signing public key substituted
    arknet-node                     the host tool
    release-signing.pub             the signing public key (canonical base64)
    releases/<version>/release.json         digest-pinned manifest
    releases/<version>/release.json.sig     Ed25519 signature (base64)
    releases/<version>/compose.yaml, compose.transfer.yaml
"""
from __future__ import annotations

import base64
import hashlib
import importlib.machinery
import importlib.util
import json
import os
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
NODE_COMPONENTS = ("arknet-agent", "arknet-ui", "arknet-transfer")
# Every ArkNet image and its build context, relative to the repository root.
COMPONENTS = {"arknet-core": "core", "arknet-tracker": "tracker", "arknet-relay": "relay",
              "arknet-agent": "node-agent", "arknet-transfer": "transfer", "arknet-ui": "node-ui"}
NODE_COMPOSE_FILES = ("compose.yaml", "compose.transfer.yaml")
PINNED = re.compile(r"^[^\s@]+:[0-9A-Za-z_][0-9A-Za-z_.-]*@sha256:[0-9a-f]{64}$")
SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$")


def load_arknet_node(node_dir: Path = ROOT / "deploy" / "node"):
    """The host tool as a module: its verifier checks what we publish."""
    loader = importlib.machinery.SourceFileLoader("arknet_node_for_release", str(node_dir / "arknet-node"))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


def channel_for(version: str) -> str:
    """Tagged releases without a pre-release suffix are stable; others development."""
    match = SEMVER.match(version)
    if not match:
        raise ValueError(f"not a SemVer version: {version}")
    return "development" if match.group(4) else "stable"


def openssl3() -> str:
    """OpenSSL 3 is needed for Ed25519 `pkeyutl -rawin` (macOS ships LibreSSL)."""
    for directory in (None, "/opt/homebrew/opt/openssl@3/bin", "/usr/local/opt/openssl@3/bin"):
        binary = shutil.which("openssl", path=directory)
        if binary and re.match(r"OpenSSL [3-9]\.", subprocess.run([binary, "version"], capture_output=True, text=True).stdout):
            return binary
    raise RuntimeError("OpenSSL 3 or newer is required (brew install openssl@3)")


def public_key(private_key: Path, openssl: str | None = None) -> str:
    """Canonical base64 of the raw Ed25519 public key for a private key file."""
    der = subprocess.run([openssl or openssl3(), "pkey", "-in", str(private_key), "-pubout", "-outform", "DER"],
                         check=True, capture_output=True).stdout
    if len(der) != 44 or not der.startswith(bytes.fromhex("302a300506032b6570032100")):
        raise ValueError("the signing key is not an Ed25519 key")
    return base64.b64encode(der[-32:]).decode()


def check_private_key(path: Path, repository: Path = ROOT) -> None:
    """The signing key lives in the operator's shell environment, never the repository."""
    path = path.expanduser()
    if not path.is_file() or path.is_symlink():
        raise ValueError(f"signing key {path} is not a regular file")
    resolved = path.resolve()
    if resolved == repository.resolve() or repository.resolve() in resolved.parents:
        raise ValueError("the signing key must not be inside the repository")
    if path.stat().st_mode & 0o077:
        raise ValueError(f"signing key {path} must be readable only by its owner (chmod 600)")


def sign(data: bytes, private_key: Path, domain: bytes, openssl: str | None = None) -> bytes:
    """release.json.sig: base64 of the Ed25519 signature over domain + data."""
    with tempfile.TemporaryDirectory(prefix="arknet-sign-") as work:
        message, signature = Path(work) / "message", Path(work) / "signature"
        message.write_bytes(domain + data)
        subprocess.run([openssl or openssl3(), "pkeyutl", "-sign", "-inkey", str(private_key), "-rawin",
                        "-in", str(message), "-out", str(signature)], check=True, capture_output=True)
        raw = signature.read_bytes()
    if len(raw) != 64:
        raise ValueError("unexpected Ed25519 signature length")
    return base64.b64encode(raw) + b"\n"


def compose_images(text: str) -> list[str]:
    return re.findall(r"(?m)^\s+image:\s*(\S+)\s*$", text)


def node_release(version: str, images: dict[str, str], project: str, published_at: str,
                 node_dir: Path = ROOT / "deploy" / "node") -> dict[str, bytes]:
    """Digest-pinned Compose files and release.json for one Node release.

    `images` maps each of NODE_COMPONENTS to `<repository>:<version>@sha256:<digest>`.
    Third-party images (PostgreSQL, Jellyfin) come from the checked-in files,
    which must already pin them by digest."""
    channel = channel_for(version)
    for component in NODE_COMPONENTS:
        ref = images.get(component, "")
        if not PINNED.match(ref) or ref.split("@")[0].rsplit(":", 1)[1] != version:
            raise ValueError(f"{component} image must be <repository>:{version}@sha256:<digest>, got {ref!r}")
    files: dict[str, bytes] = {}
    for name in NODE_COMPOSE_FILES:
        text = (node_dir / name).read_text()
        text = re.sub(r"(?m)^name: .*$", f"name: {project}", text)
        text = re.sub(r"(?m)^    build:\n      context: .*\n", "", text)
        for component in NODE_COMPONENTS:
            text = re.sub(rf"(?m)^(    image: ){re.escape(component)}:\S+$", lambda m: m.group(1) + images[component], text)
        unpinned = [image for image in compose_images(text) if not PINNED.match(image)]
        if unpinned or "build:" in text:
            raise ValueError(f"{name}: release Compose files must use only digest-pinned images ({unpinned})")
        files[name] = text.encode()
    manifest = json.loads((node_dir / "release.json").read_text())
    manifest.update(version=version, channel=channel, published_at=published_at)
    for component in NODE_COMPONENTS:
        manifest["components"][component] = {"version": version, "image": images[component], "bundled": True}
    shipped = set(compose_images(files["compose.yaml"].decode()))
    for component in ("postgresql", "jellyfin"):
        if manifest["components"][component]["image"] not in shipped:
            raise ValueError(f"release.json {component} image does not match compose.yaml")
    manifest["compose"] = {"project": project, "files": [
        {"path": name, "sha256": hashlib.sha256(content).hexdigest()} for name, content in files.items()]}
    files["release.json"] = (json.dumps(manifest, indent=2) + "\n").encode()
    return files


def write_site(out: Path, version: str, files: dict[str, bytes], signature: bytes, verify_key: str,
               site_url: str, node_dir: Path = ROOT / "deploy" / "node") -> dict:
    """Write (or update) a release site directory; returns the index entry."""
    tool = load_arknet_node(node_dir)
    tool.parse_verify_key(verify_key)  # canonical base64: safe to substitute into install.sh
    if not re.match(r"^https?://[^\s\"'\\$`]+$", site_url):
        raise ValueError("site URL must be an http(s) URL without quotes, spaces or shell characters")
    release = out / "releases" / version
    if release.exists():
        raise ValueError(f"{release} already exists: published releases are never replaced")
    release.mkdir(parents=True)
    for name, content in files.items():
        (release / name).write_bytes(content)
    (release / "release.json.sig").write_bytes(signature)
    installer = (node_dir / "install.sh").read_text()
    for placeholder, value in (("__ARKNET_RELEASE_URL__", site_url.rstrip("/")), ("__ARKNET_RELEASE_VERIFY_KEY__", verify_key)):
        if installer.count(placeholder) != 1:
            raise ValueError(f"install.sh must contain {placeholder} exactly once")
        installer = installer.replace(placeholder, value)
    (out / "install.sh").write_text(installer)
    shutil.copy(node_dir / "arknet-node", out / "arknet-node")
    (out / "release-signing.pub").write_text(verify_key + "\n")
    index_path = out / "index.json"
    index = json.loads(index_path.read_text()) if index_path.exists() else {"schema": tool.INDEX_SCHEMA, "channels": {}}
    entry = {"version": version, "manifest_url": f"releases/{version}/release.json",
             "manifest_sha256": hashlib.sha256(files["release.json"]).hexdigest()}
    index["channels"][channel_for(version)] = entry
    tool.validate_index(json.dumps(index).encode())
    index_path.write_text(json.dumps(index, indent=2) + "\n")
    return entry


def verify_site_release(out: Path, version: str, verify_key: str, node_dir: Path = ROOT / "deploy" / "node") -> dict:
    """Check a published release exactly as arknet-node will."""
    tool = load_arknet_node(node_dir)
    release = out / "releases" / version
    data = (release / "release.json").read_bytes()
    tool.verify_release_signature(tool.parse_verify_key(verify_key), data, (release / "release.json.sig").read_bytes(),
                                  openssl=openssl3())
    manifest = tool.validate_manifest(data)
    tool.require_pinned_images(manifest)
    for item in manifest["compose"]["files"]:
        if hashlib.sha256((release / item["path"]).read_bytes()).hexdigest() != item["sha256"]:
            raise ValueError(f"{item['path']} does not match release.json")
    return manifest


def with_openssl_on_path(openssl: str) -> dict:
    """Environment whose `openssl` is OpenSSL 3, for running arknet-node on macOS."""
    return {**os.environ, "PATH": str(Path(openssl).parent) + os.pathsep + os.environ.get("PATH", "")}
