"""Prompt 51A: scripts/publish-release.py end to end, against throwaway resources.

Creates a temporary clone holding the current working tree, bumps VERSION,
commits a throwaway signing public key and an annotated tag, starts a local
registry and runs the real publish command for the native platform. Then:
all six images are in the registry and pullable anonymously by digest, the
site verifies as arknet-node would, and every temporary resource is removed.

Opt-in (builds six images): ARKNET_TEST_RELEASE_PUBLISH=1.
"""
import json
import os
import platform
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.request
from pathlib import Path

from scripts import release_site

ROOT = release_site.ROOT
ENABLED = os.environ.get("ARKNET_TEST_RELEASE_PUBLISH") == "1"
VERSION = "0.0.9-rc.1"
PORT = 15051


def run(*args, **kw):
    return subprocess.run(list(args), check=kw.pop("check", True), capture_output=True, text=True, **kw)


def load_publish():
    import importlib.util
    spec = importlib.util.spec_from_file_location("publish_release", ROOT / "scripts" / "publish-release.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class AnonymousPullTests(unittest.TestCase):
    """GHCR answers a private package's anonymous token request with 401."""

    def test_private_package_is_reported_not_raised(self):
        import email.message
        import urllib.error
        from unittest import mock
        publish = load_publish()
        challenge = email.message.Message()
        challenge["WWW-Authenticate"] = 'Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:nasible/arknet-core:pull"'

        def urlopen(request, timeout=0):
            url = request if isinstance(request, str) else request.full_url
            raise urllib.error.HTTPError(url, 401, "Unauthorized", challenge, None)

        with mock.patch.object(publish.urllib.request, "urlopen", urlopen):
            self.assertFalse(publish.anonymous_pull_ok("ghcr.io/nasible/arknet-core", "sha256:" + "0" * 64))


@unittest.skipUnless(ENABLED, "set ARKNET_TEST_RELEASE_PUBLISH=1 to build and publish a throwaway release")
class PublishReleaseTest(unittest.TestCase):
    def test_publish_builds_pushes_signs_and_verifies(self):
        registry_name = "arknet-publish-test-" + secrets.token_hex(4)
        registry = f"localhost:{PORT}/arknet-test"
        work = Path(tempfile.mkdtemp(prefix="arknet-publish-test-"))
        try:
            openssl = release_site.openssl3()
            key = work / "signing.pem"
            run(openssl, "genpkey", "-algorithm", "ed25519", "-out", str(key))
            key.chmod(0o600)
            public = release_site.public_key(key, openssl)
            clone = work / "repo"
            run("git", "clone", "-q", "--no-hardlinks", str(ROOT), str(clone))
            # The tree under test is the working tree, not only the last commit.
            for path in run("git", "-C", str(ROOT), "ls-files", "-co", "--exclude-standard").stdout.split("\n"):
                if path and (ROOT / path).is_file():
                    target = clone / path
                    target.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copy2(ROOT / path, target)
            for path in run("git", "-C", str(ROOT), "ls-files", "--deleted").stdout.split():
                run("git", "-C", str(clone), "rm", "-q", "--ignore-unmatch", path)
            (clone / "VERSION").write_text(VERSION + "\n")
            (clone / "deploy/node/release-signing.pub").write_text(public + "\n")
            run("git", "-C", str(clone), "add", "-A")
            run("git", "-C", str(clone), "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-q", "-m", "test release")
            run("git", "-C", str(clone), "-c", "user.name=test", "-c", "user.email=test@invalid", "tag", "-a", f"v{VERSION}", "-m", "test")
            run("docker", "run", "-d", "--name", registry_name, "-p", f"127.0.0.1:{PORT}:5000", "registry:latest")
            native = "linux/arm64" if platform.machine() in ("arm64", "aarch64") else "linux/amd64"
            site = work / "site"
            result = subprocess.run([sys.executable, str(ROOT / "scripts" / "publish-release.py"), "publish", "--tag", f"v{VERSION}",
                                     "--repo", str(clone), "--signing-key", str(key), "--site-url", "https://releases.example/node",
                                     "--out", str(site), "--registry", registry, "--platforms", native,
                                     "--insecure-registry", f"localhost:{PORT}"], capture_output=True, text=True, timeout=7200)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 0, output[-4000:])
            self.assertIn(f"RELEASED: ArkNet {VERSION} (development)", output)
            self.assertNotIn("PRIVATE KEY", output)
            record = json.loads((site / "releases" / VERSION / "images.json").read_text())
            self.assertEqual(set(record["images"]), set(release_site.COMPONENTS))
            self.assertEqual(record["commit"], run("git", "-C", str(clone), "rev-parse", "HEAD").stdout.strip())
            for component, ref in record["images"].items():
                self.assertRegex(ref, rf"^{registry}/{component}:{VERSION}@sha256:[0-9a-f]{{64}}$")
                digest = ref.split("@")[1]
                request = urllib.request.Request(f"http://localhost:{PORT}/v2/arknet-test/{component}/manifests/{digest}", method="HEAD",
                                                 headers={"Accept": "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json"})
                with urllib.request.urlopen(request, timeout=10) as response:
                    self.assertEqual(response.headers["Docker-Content-Digest"], digest)
            manifest = release_site.verify_site_release(site, VERSION, public)
            self.assertEqual(manifest["components"]["arknet-agent"]["image"], record["images"]["arknet-agent"])
            self.assertEqual(json.loads((site / "index.json").read_text())["channels"]["development"]["version"], VERSION)
            pulled = record["images"]["arknet-agent"]
            run("docker", "pull", "-q", pulled)
            labels = json.loads(run("docker", "image", "inspect", "--format", "{{json .Config.Labels}}", pulled).stdout)
            self.assertEqual(labels["org.opencontainers.image.version"], VERSION)
            self.assertEqual(labels["org.opencontainers.image.revision"], record["commit"])
            run("docker", "rmi", pulled, check=False)
            # A second publish of the same tag must not replace the published release.
            again = subprocess.run([sys.executable, str(ROOT / "scripts" / "publish-release.py"), "publish", "--tag", f"v{VERSION}",
                                    "--repo", str(clone), "--signing-key", str(key), "--site-url", "https://releases.example/node",
                                    "--out", str(site), "--registry", registry, "--platforms", native,
                                    "--insecure-registry", f"localhost:{PORT}"], capture_output=True, text=True, timeout=7200)
            self.assertNotEqual(again.returncode, 0)
            self.assertIn("never replaced", again.stdout + again.stderr)
        finally:
            run("docker", "rm", "-f", "-v", registry_name, check=False)
            time.sleep(1)
            builders = [l.split()[0] for l in run("docker", "buildx", "ls", check=False).stdout.splitlines()
                        if l.startswith("arknet-release-")]
            for builder in builders:
                run("docker", "buildx", "rm", "--force", builder, check=False)
            shutil.rmtree(work, ignore_errors=True)
            self.assertEqual(builders, [], "publish-release.py left a buildx builder behind")


if __name__ == "__main__":
    unittest.main()
