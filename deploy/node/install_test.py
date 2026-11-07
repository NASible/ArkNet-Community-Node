"""install.sh pre-flight checks under Ubuntu's dash (requires Docker).

The full install is exercised on a fresh VM by scripts/check-node-install-vm.py.
"""
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
IMAGE = "ubuntu:24.04"
UBUNTU = 'ID=ubuntu\nVERSION_ID="24.04"\nPRETTY_NAME="Ubuntu 24.04.3 LTS"\n'
MEMINFO = "MemTotal:        4015148 kB\n"
KEY = "t7S7sCEzbftgFj3ukmqGKlgfXA70Zkr37xCcOckyWNQ="  # a public test key (node-agent release test vectors)


def installer(key=KEY):
    """install.sh as the release publisher ships it: the signing key substituted."""
    # Inside the checkout (git-ignored .arknet-*): Colima shares it reliably,
    # while files under TMPDIR can appear empty to containers.
    handle = tempfile.NamedTemporaryFile("w", suffix=".sh", prefix=".arknet-install-test-", delete=False, dir=HERE.parent.parent)
    with handle:
        handle.write((HERE / "install.sh").read_text().replace("__ARKNET_RELEASE_VERIFY_KEY__", key))
    os.chmod(handle.name, 0o644)
    return handle.name


def run(args, os_release=UBUNTU, meminfo=MEMINFO, uid="0", key=KEY):
    script = (f"printf '%s' '{os_release}' > /tmp/os-release && printf '%s' '{meminfo}' > /tmp/meminfo && "
              f"ARKNET_INSTALL_OS_RELEASE=/tmp/os-release ARKNET_INSTALL_MEMINFO=/tmp/meminfo ARKNET_INSTALL_ROOT_UID={uid} "
              f"sh /install.sh {args}")
    path = installer(key) if key is not None else str(HERE / "install.sh")
    try:
        result = subprocess.run(["docker", "run", "--rm", "--network", "none", "-v", f"{path}:/install.sh:ro",
                                 IMAGE, "sh", "-c", script], capture_output=True, text=True, timeout=120)
    finally:
        if key is not None:
            os.unlink(path)
    return result.returncode, result.stdout + result.stderr


REL = "--release-url https://releases.arknet.example/node"


class InstallerPreflight(unittest.TestCase):
    def assertFails(self, args, *expected, **kw):
        code, out = run(args, **kw)
        self.assertNotEqual(code, 0, out)
        self.assertIn("INSTALL FAILED", out)
        for text in expected:
            self.assertIn(text, out)
        return out

    def test_requires_root_and_repeats_options_in_hint(self):
        self.assertFails(f"{REL} --core-url https://core.example", "must run as root",
                         "sudo sh install.sh --release-url https://releases.arknet.example/node --core-url https://core.example", uid="12345")

    def test_rejects_unsupported_os(self):
        self.assertFails(REL, "Unsupported operating system: Debian GNU/Linux 12", "Ubuntu Server LTS 22.04 24.04 26.04",
                         os_release='ID=debian\nVERSION_ID="12"\nPRETTY_NAME="Debian GNU/Linux 12 (bookworm)"\n')
        self.assertFails(REL, "Unsupported operating system: Ubuntu 25.10",
                         os_release='ID=ubuntu\nVERSION_ID="25.10"\nPRETTY_NAME="Ubuntu 25.10"\n')

    def test_rejects_low_memory(self):
        self.assertFails(REL, "Not enough memory: 976 MiB", "at least 2 GB of RAM", meminfo="MemTotal: 1000000 kB\n")

    def test_requires_release_site(self):
        self.assertFails("", "does not know its ArkNet release site", "--release-url")

    def test_url_rules(self):
        self.assertFails(f"{REL} --core-url http://core.example", "--core-url must use https://", "--allow-insecure-http")
        self.assertFails("--release-url http://releases.example", "--release-url must use https://")
        self.assertFails(f"{REL} --core-url https://core.example/api", "no path")
        self.assertFails(f"{REL} --core-url https://u:p@core.example", "no path, query or credentials")
        self.assertFails(f"{REL} --core-url http://127.0.0.1:8080", "loopback", "cannot reach Core there")
        self.assertFails(f"{REL} --core-url 'https://core.example/\\$(reboot)'", "not allowed")
        self.assertFails(f"{REL} --core-url ftp://core.example", "must be an http(s) URL")

    def test_requires_a_well_formed_release_signing_key(self):
        self.assertFails(REL, "does not contain the ArkNet release signing key", key=None)
        for bad in ("c2hvcnQ=", KEY[:-2] + "B=", KEY.rstrip("="), "x" * 43 + "=", "$(reboot)"):
            with self.subTest(bad=bad):
                self.assertFails(REL, "release signing key in this install.sh is malformed", key=bad)

    def test_rejects_bad_options(self):
        self.assertFails(f"{REL} --channel beta", "--channel must be stable or development")
        self.assertFails(f"{REL} --home relative", "--home must be an absolute path")
        self.assertFails(f"{REL} --bogus", "Unknown option: --bogus")

    def test_supported_machine_passes_preflight(self):
        # The container has no network; the first network step is the Docker/APT stage.
        code, out = run(f"{REL} --core-url https://core.example --no-install-docker")
        self.assertIn("OS: Ubuntu 24.04.3 LTS", out)
        self.assertIn("Memory: 3921 MiB", out)
        self.assertNotIn("Unsupported", out)
        # Next is the disk check: either it passes or it fails with an actionable message.
        self.assertTrue("Free disk:" in out or ("Not enough free disk space" in out and "20 GiB" in out), out)


if __name__ == "__main__":
    unittest.main()
