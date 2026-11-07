#!/usr/bin/env python3
"""Build, push and sign an ArkNet release from a clean checkout of a tag (Prompt 51A).

    scripts/publish-release.py public-key --signing-key ~/.arknet/release-signing.key
    scripts/publish-release.py publish --tag v0.1.0 --signing-key ~/.arknet/release-signing.key \\
        --site-url https://nasible.github.io/ArkNet-Community-Node --out ~/arknet-release-site

`publish`:
  1. exports the annotated tag with `git archive` (no working-tree files);
  2. builds all six ArkNet images (core, tracker, relay, agent, transfer, ui)
     with BuildKit for every --platform and pushes them to --registry;
  3. records each pushed digest and checks the image can be pulled anonymously;
  4. writes a digest-pinned Node release (compose files + release.json), signs
     release.json with the Ed25519 key, verifies the signature exactly as
     arknet-node will, and updates the static release site in --out.

The signing key is read only from the path given (outside the repository,
mode 0600); it is never copied or printed. Registry credentials come from the
operator's existing `docker login`. See docs/release-process.md.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import re
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from scripts import release_site  # noqa: E402

SOURCE_URL = "https://github.com/NASible/ArkNet-Community-Node"
DEFAULT_REGISTRY = "ghcr.io/nasible"
DEFAULT_PLATFORMS = "linux/amd64,linux/arm64"
MANIFEST_TYPES = ", ".join(["application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json",
                            "application/vnd.docker.distribution.manifest.list.v2+json",
                            "application/vnd.docker.distribution.manifest.v2+json"])


def run(*args, **kw):
    kw.setdefault("check", True)
    kw.setdefault("capture_output", True)
    kw.setdefault("text", True)
    return subprocess.run(list(args), **kw)


def say(message):
    print(message, flush=True)


def export_tag(repo: Path, tag: str, into: Path) -> str:
    """Clean tree of an annotated tag; returns the tagged commit."""
    if run("git", "-C", str(repo), "cat-file", "-t", f"refs/tags/{tag}", check=False).stdout.strip() != "tag":
        raise SystemExit(f"{tag} is not an annotated tag in {repo} (create it with `git tag -a {tag}`)")
    commit = run("git", "-C", str(repo), "rev-list", "-n", "1", tag).stdout.strip()
    into.mkdir()
    archive = subprocess.Popen(["git", "-C", str(repo), "archive", "--format=tar", tag], stdout=subprocess.PIPE)
    extract = subprocess.run(["tar", "-x", "-C", str(into)], stdin=archive.stdout)
    archive.stdout.close()
    if archive.wait() != 0 or extract.returncode != 0:
        raise SystemExit(f"could not export {tag}")
    return commit


def registry_host(repository: str) -> tuple[str, str]:
    host, _, path = repository.partition("/")
    return host, path


def anonymous_pull_ok(repository: str, digest: str) -> bool:
    """HEAD the manifest with no credentials, using an anonymous bearer token if offered."""
    host, path = registry_host(repository)
    scheme = "http" if host.split(":")[0] in ("localhost", "127.0.0.1") else "https"
    url = f"{scheme}://{host}/v2/{path}/manifests/{digest}"
    headers = {"Accept": MANIFEST_TYPES}
    for _ in range(2):
        try:
            with urllib.request.urlopen(urllib.request.Request(url, method="HEAD", headers=headers), timeout=30) as response:
                return response.status == 200
        except urllib.error.HTTPError as error:
            challenge = error.headers.get("WWW-Authenticate", "")
            if error.code != 401 or not challenge.lower().startswith("bearer ") or "Authorization" in headers:
                return False
            fields = dict(re.findall(r'(\w+)="([^"]*)"', challenge))
            query = urllib.parse.urlencode({k: v for k, v in fields.items() if k in ("service", "scope")})
            try:
                with urllib.request.urlopen(f"{fields['realm']}?{query}", timeout=30) as response:
                    token = json.loads(response.read()).get("token") or ""
            except urllib.error.HTTPError:
                return False  # GHCR refuses anonymous tokens for private packages
            headers["Authorization"] = f"Bearer {token}"
    return False


def create_builder(insecure: list[str], work: Path) -> str:
    name = "arknet-release-" + secrets.token_hex(4)
    args = ["docker", "buildx", "create", "--name", name, "--driver", "docker-container",
            "--driver-opt", "network=host", "--bootstrap"]
    if insecure:  # throwaway test registries only
        config = work / "buildkitd.toml"
        config.write_text("".join(f'[registry."{host}"]\n  http = true\n  insecure = true\n' for host in insecure))
        args += ["--buildkitd-config", str(config)]
    run(*args, timeout=600)
    return name


def build_and_push(builder: str, source: Path, component: str, context: str, ref: str, version: str, commit: str,
                   platforms: str, work: Path) -> str:
    metadata = work / f"{component}.metadata.json"
    result = run("docker", "buildx", "build", "--builder", builder, "--platform", platforms, "--push",
        "--provenance=false", "--sbom=false", "--metadata-file", str(metadata),
        "--build-arg", f"ARKNET_VERSION={version}",
        "--label", f"org.opencontainers.image.source={SOURCE_URL}",
        "--label", f"org.opencontainers.image.version={version}",
        "--label", f"org.opencontainers.image.revision={commit}",
        "-t", ref, str(source / context), timeout=7200, check=False)
    if result.returncode != 0:
        tail = "\n".join((result.stdout + result.stderr).strip().splitlines()[-25:])
        raise SystemExit(f"building {component} failed:\n{tail}")
    digest = json.loads(metadata.read_text()).get("containerimage.digest", "")
    if not re.match(r"^sha256:[0-9a-f]{64}$", digest):
        raise SystemExit(f"no pushed digest recorded for {component}")
    return digest


def publish(args) -> int:
    repo, out = args.repo.resolve(), args.out.expanduser().resolve()
    tag, registry = args.tag, args.registry.rstrip("/")
    if not re.match(r"^v\d", tag):
        raise SystemExit("release tags look like v<version>, for example v0.1.0")
    version = tag[1:]
    channel = release_site.channel_for(version)
    if repo == out or repo in out.parents:
        raise SystemExit("--out must be outside the repository")
    if (out / "releases" / version).exists():
        raise SystemExit(f"{out / 'releases' / version} already exists: published releases are never replaced")
    key = args.signing_key.expanduser()
    release_site.check_private_key(key, repo)
    openssl = release_site.openssl3()
    signing_public = release_site.public_key(key, openssl)
    started = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
    with tempfile.TemporaryDirectory(prefix="arknet-release-") as temporary:
        work = Path(temporary)
        source = work / "source"
        commit = export_tag(repo, tag, source)
        say(f"[1/5] Exported {tag} ({commit[:12]}): ArkNet {version} ({channel} channel)")
        if (source / "VERSION").read_text().strip() != version:
            raise SystemExit(f"VERSION in {tag} is not {version}; bump it in the tagged commit")
        shipped_key = source / "deploy" / "node" / "release-signing.pub"
        if not shipped_key.is_file() or shipped_key.read_text().strip() != signing_public:
            raise SystemExit(f"deploy/node/release-signing.pub in {tag} does not match the signing key "
                             f"(expected {signing_public}); see docs/release-process.md")
        builder = create_builder(args.insecure_registry, work)
        images = {}
        try:
            say(f"[2/5] Building and pushing six images for {args.platforms} to {registry}")
            for component, context in release_site.COMPONENTS.items():
                repository = f"{registry}/{component}"
                digest = build_and_push(builder, source, component, context, f"{repository}:{version}", version, commit,
                                        args.platforms, work)
                images[component] = f"{repository}:{version}@{digest}"
                say(f"      {images[component]}")
        finally:
            run("docker", "buildx", "rm", "--force", builder, check=False, timeout=120)
        say("[3/5] Checking every image can be pulled anonymously")
        deadline = time.monotonic() + args.wait_public
        while True:
            private = [ref for ref in images.values()
                       if not anonymous_pull_ok(ref.split(":" + version + "@")[0], ref.split("@")[1])]
            if not private or time.monotonic() >= deadline:
                break
            say(f"      waiting for {len(private)} package(s) to become public: "
                + ", ".join(ref.split(":" + version)[0] for ref in private))
            time.sleep(15)
        if private:
            raise SystemExit("not anonymously pullable; make the packages public (GitHub: package settings -> "
                             f"Change visibility) and re-run with the same tag: {private}")
        say("[4/5] Writing and signing the digest-pinned Node release")
        node_dir = source / "deploy" / "node"
        files = release_site.node_release(version, images, args.project, started.strftime("%Y-%m-%dT%H:%M:%SZ"), node_dir)
        tool = release_site.load_arknet_node(node_dir)
        signature = release_site.sign(files["release.json"], key, tool.SIGNATURE_DOMAIN, openssl)
        out.mkdir(parents=True, exist_ok=True)
        entry = release_site.write_site(out, version, files, signature, signing_public, args.site_url, node_dir)
        (out / "releases" / version / "images.json").write_text(json.dumps(
            {"version": version, "tag": tag, "commit": commit, "platforms": args.platforms.split(","),
             "published_at": started.strftime("%Y-%m-%dT%H:%M:%SZ"), "images": images}, indent=2) + "\n")
        say("[5/5] Verifying the published release as arknet-node does")
        release_site.verify_site_release(out, version, signing_public, node_dir)
    say(f"RELEASED: ArkNet {version} ({channel}); manifest SHA-256 {entry['manifest_sha256']}")
    say(f"Upload {out} to {args.site_url} (see docs/release-process.md).")
    return 0


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = parser.add_subparsers(dest="command", required=True)
    pub = sub.add_parser("public-key", help="print the canonical base64 public key of a signing key")
    pub.add_argument("--signing-key", type=Path, required=True)
    rel = sub.add_parser("publish", help="build, push, sign and write the release site")
    rel.add_argument("--tag", required=True)
    rel.add_argument("--signing-key", type=Path, required=True, help="Ed25519 private key (PEM), outside the repository")
    rel.add_argument("--site-url", required=True, help="where the release site will be served")
    rel.add_argument("--out", type=Path, required=True, help="release site directory (created or updated)")
    rel.add_argument("--registry", default=DEFAULT_REGISTRY)
    rel.add_argument("--platforms", default=DEFAULT_PLATFORMS)
    rel.add_argument("--wait-public", type=int, default=0, metavar="SECONDS",
                     help="keep re-checking anonymous pulls this long (new GHCR packages start private)")
    rel.add_argument("--project", default="arknet-node", help="Compose project name of installed Nodes")
    rel.add_argument("--repo", type=Path, default=release_site.ROOT)
    rel.add_argument("--insecure-registry", action="append", default=[], help=argparse.SUPPRESS)  # test registries
    args = parser.parse_args(argv)
    if args.command == "public-key":
        release_site.check_private_key(args.signing_key.expanduser())
        print(release_site.public_key(args.signing_key.expanduser()))
        return 0
    return publish(args)


if __name__ == "__main__":
    sys.exit(main())
