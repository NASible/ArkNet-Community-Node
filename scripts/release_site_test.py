"""Prompt 51A: release-site generator, signing and image pinning (offline)."""
import hashlib
import json
import re
import subprocess
import tempfile
import unittest
from pathlib import Path

from scripts import release_site

ROOT = release_site.ROOT
VERSION = "0.1.0"


def refs(version=VERSION, registry="ghcr.io/nasible"):
    return {c: f"{registry}/{c}:{version}@sha256:{hashlib.sha256((c + version).encode()).hexdigest()}"
            for c in release_site.NODE_COMPONENTS}


class Keys:
    def __init__(self):
        self.dir = tempfile.TemporaryDirectory()
        self.openssl = release_site.openssl3()
        self.keys = {}
        for name in ("release", "other"):
            path = Path(self.dir.name) / f"{name}.pem"
            subprocess.run([self.openssl, "genpkey", "-algorithm", "ed25519", "-out", str(path)], check=True, capture_output=True)
            path.chmod(0o600)
            self.keys[name] = (path, release_site.public_key(path, self.openssl))


class GeneratorTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        try:
            cls.k = Keys()
        except RuntimeError as error:
            raise unittest.SkipTest(str(error))
        cls.tool = release_site.load_arknet_node()

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.out = Path(self.tmp.name) / "site"

    def publish(self, version=VERSION, key="release", files=None):
        files = files or release_site.node_release(version, refs(version), "arknet-node", "2026-11-07T00:00:00Z")
        path, public = self.k.keys[key]
        signature = release_site.sign(files["release.json"], path, self.tool.SIGNATURE_DOMAIN, self.k.openssl)
        return release_site.write_site(self.out, version, files, signature, self.k.keys["release"][1],
                                       "https://releases.example/node"), files

    def test_channel_follows_the_version(self):
        self.assertEqual(release_site.channel_for("0.1.0"), "stable")
        self.assertEqual(release_site.channel_for("0.2.0-rc.1"), "development")
        self.assertRaises(ValueError, release_site.channel_for, "0.1")

    def test_node_release_is_digest_pinned_and_valid_for_arknet_node(self):
        files = release_site.node_release(VERSION, refs(), "arknet-node", "2026-11-07T00:00:00Z")
        for name in release_site.NODE_COMPOSE_FILES:
            text = files[name].decode()
            self.assertNotIn("build:", text)
            images = release_site.compose_images(text)
            self.assertTrue(images)
            for image in images:
                self.assertRegex(image, release_site.PINNED)
        self.assertIn("name: arknet-node", files["compose.yaml"].decode())
        manifest = self.tool.validate_manifest(files["release.json"])
        self.tool.require_pinned_images(manifest)
        self.assertEqual((manifest["version"], manifest["channel"], manifest["compose"]["project"]), (VERSION, "stable", "arknet-node"))
        self.assertEqual(manifest["components"]["arknet-agent"]["image"], refs()["arknet-agent"])
        for item in manifest["compose"]["files"]:
            self.assertEqual(item["sha256"], hashlib.sha256(files[item["path"]]).hexdigest())

    def test_node_release_rejects_unpinned_or_mismatched_images(self):
        for bad in ({**refs(), "arknet-ui": "ghcr.io/nasible/arknet-ui:0.1.0"},
                    {**refs(), "arknet-agent": refs("0.1.1")["arknet-agent"]},
                    {k: v for k, v in refs().items() if k != "arknet-transfer"}):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                release_site.node_release(VERSION, bad, "arknet-node", "2026-11-07T00:00:00Z")

    def test_signed_site_verifies_like_arknet_node_and_install_sh_is_complete(self):
        entry, files = self.publish()
        release_site.verify_site_release(self.out, VERSION, self.k.keys["release"][1])
        index = json.loads((self.out / "index.json").read_text())
        self.assertEqual(index["channels"]["stable"], entry)
        self.assertEqual(entry["manifest_sha256"], hashlib.sha256(files["release.json"]).hexdigest())
        installer = (self.out / "install.sh").read_text()
        self.assertNotIn("__ARKNET_", installer)
        self.assertIn(f"RELEASE_VERIFY_KEY='{self.k.keys['release'][1]}'", installer)
        self.assertIn('RELEASE_URL_DEFAULT="https://releases.example/node"', installer)
        self.assertEqual((self.out / "release-signing.pub").read_text().strip(), self.k.keys["release"][1])
        self.assertEqual((self.out / "arknet-node").read_bytes(), (ROOT / "deploy/node/arknet-node").read_bytes())

    def test_tampered_or_foreign_signed_release_fails_verification(self):
        self.publish()
        release = self.out / "releases" / VERSION
        original = (release / "release.json").read_bytes()
        (release / "release.json").write_bytes(original.replace(b'"arknet-node"', b'"arknet-evil"', 1))
        self.assertRaises(ValueError, release_site.verify_site_release, self.out, VERSION, self.k.keys["release"][1])
        (release / "release.json").write_bytes(original)
        self.assertRaises(ValueError, release_site.verify_site_release, self.out, VERSION, self.k.keys["other"][1])
        other = Path(self.tmp.name) / "other-site"
        self.out = other
        self.publish(key="other")  # signed by the wrong key, published with the release key
        self.assertRaises(ValueError, release_site.verify_site_release, other, VERSION, self.k.keys["release"][1])

    def test_index_keeps_other_channels_and_never_replaces_a_release(self):
        self.publish("0.2.0-rc.1")
        self.publish(VERSION)
        channels = json.loads((self.out / "index.json").read_text())["channels"]
        self.assertEqual({c: e["version"] for c, e in channels.items()}, {"development": "0.2.0-rc.1", "stable": VERSION})
        with self.assertRaises(ValueError):
            self.publish(VERSION)

    def test_signing_key_must_be_private_and_outside_the_repository(self):
        path = self.k.keys["release"][0]
        release_site.check_private_key(path)
        path.chmod(0o644)
        try:
            self.assertRaises(ValueError, release_site.check_private_key, path)
        finally:
            path.chmod(0o600)
        self.assertRaises(ValueError, release_site.check_private_key, ROOT / "VERSION")
        self.assertRaises(ValueError, release_site.check_private_key, Path(self.tmp.name) / "missing.pem")


class DockerfileTests(unittest.TestCase):
    """Multi-arch releases cross-compile natively (Go crashes under QEMU emulation)."""

    def test_build_stages_run_on_the_build_platform(self):
        for component, context in release_site.COMPONENTS.items():
            text = (ROOT / context / "Dockerfile").read_text()
            first = re.search(r"(?m)^FROM .*$", text).group(0)
            with self.subTest(component=component):
                self.assertIn("--platform=$BUILDPLATFORM", first)
                if "golang:" in first:
                    self.assertIn("GOOS=$TARGETOS GOARCH=$TARGETARCH go build", text)
                    self.assertRegex(text, r"(?m)^ARG TARGETOS TARGETARCH$")


class VersionStampTests(unittest.TestCase):
    """Released binaries report the tag's version, not the source default."""

    SOURCES = {"core": "core/handlers.go", "agent": "node-agent/handlers.go", "transfer": "transfer/http.go"}

    def test_versioned_services_take_the_release_version_at_build_time(self):
        for component, source in self.SOURCES.items():
            context = release_site.COMPONENTS["arknet-" + component]
            dockerfile = (ROOT / context / "Dockerfile").read_text()
            with self.subTest(component=component):
                self.assertRegex((ROOT / source).read_text(), r'(?m)^var version = "[^"]+"$',
                                 "-X main.version cannot override a const")
                self.assertRegex(dockerfile, r"(?m)^ARG ARKNET_VERSION=")
                self.assertIn("${ARKNET_VERSION:+-X main.version=$ARKNET_VERSION}", dockerfile)


class ComposePinningTests(unittest.TestCase):
    """No Compose file references an unpinned image (Prompt 51A).

    Third-party images carry a digest. ArkNet's own images are either built
    from this checkout in the same service (`build:`, development stacks) or,
    in published releases, pinned by digest (checked by the generator test)."""

    def test_every_compose_image_is_pinned_or_built_from_source(self):
        tracked = subprocess.run(["git", "-C", str(ROOT), "ls-files"], check=True, capture_output=True, text=True).stdout.split()
        compose = [p for p in tracked if re.search(r"(^|/)compose[^/]*\.ya?ml$", p)]
        self.assertGreaterEqual(len(compose), 9)
        problems = []
        for path in compose:
            for service, body in re.findall(r"(?ms)^  ([a-z0-9-]+):\n(.*?)(?=^  [a-z0-9-]+:\n|^\S|\Z)", (ROOT / path).read_text()):
                image = re.search(r"(?m)^    image:\s*(\S+)", body)
                if not image:
                    continue
                ref = image.group(1)
                built = re.search(r"(?m)^    build:", body) is not None
                if release_site.PINNED.match(ref) or (built and re.match(r"^arknet-[a-z-]+:[0-9A-Za-z_.-]+$", ref)):
                    continue
                problems.append(f"{path}: {service} uses {ref}")
        self.assertEqual(problems, [])


if __name__ == "__main__":
    unittest.main()
