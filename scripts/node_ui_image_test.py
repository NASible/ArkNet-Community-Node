"""Prompt 51: the shipped Node UI image is a production build.

Builds the real `node-ui` image and inspects it: no Vite (or any other
development dependency), no source maps, no TypeScript sources, a hashed static
build, and a server that drops root and answers on 5173. Opt-in because it
builds an image: set ARKNET_TEST_UI_IMAGE=1 (the gate's `node-ui-image` step).
"""
from pathlib import Path
import json
import os
import secrets
import subprocess
import time
import unittest

ROOT = Path(__file__).resolve().parent.parent
ENABLED = os.environ.get("ARKNET_TEST_UI_IMAGE") == "1"


def docker(*args, timeout=600, check=True):
    return subprocess.run(["docker", *args], capture_output=True, text=True, timeout=timeout, check=check)


@unittest.skipUnless(ENABLED, "set ARKNET_TEST_UI_IMAGE=1 to build and inspect the UI image")
class NodeUIImageTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tag = "arknet-ui-image-test-" + secrets.token_hex(6)
        docker("build", "-q", "-t", cls.tag, str(ROOT / "node-ui"), timeout=900)

    @classmethod
    def tearDownClass(cls):
        docker("image", "rm", "-f", cls.tag, check=False)

    def shell(self, script):
        return docker("run", "--rm", "--network", "none", "--entrypoint", "sh", self.tag, "-c", script).stdout

    def test_runtime_image_has_no_vite_dev_dependencies_or_source_maps(self):
        found = self.shell(
            "find / -xdev \\( -path /proc -o -path /sys -o -path /dev \\) -prune -o "
            "\\( -name '*.map' -o -name '*.tsx' -o -name '*.ts' -o -type d -name 'vite' -o -type d -name 'vitest' "
            "-o -type d -name '@vitejs' -o -type d -name 'typescript' -o -type d -name 'esbuild' -o -type d -name 'rollup' "
            "-o -type d -name 'rolldown' \\) -print")
        self.assertEqual(found.split(), [], "development artifacts in the runtime image")
        self.assertEqual(self.shell("ls -A /app").split(), ["dist", "server.mjs", "start-ui.sh"])

    def test_static_build_references_hashed_assets_not_sources(self):
        index = self.shell("cat /app/dist/index.html")
        self.assertIn("/assets/", index)
        self.assertNotIn("/src/main.tsx", index)

    def test_package_managers_are_absent_from_runtime(self):
        self.assertEqual(self.shell("for t in npm npx corepack yarn pnpm; do command -v $t; done; true").split(), [])

    def test_server_drops_root_and_serves_the_build(self):
        name = self.tag + "-run"
        docker("run", "-d", "--name", name, "--network", "none", "--read-only", "--tmpfs", "/tmp:size=8m",
               "--cap-drop", "ALL", "--cap-add", "SETUID", "--cap-add", "SETGID",
               "--security-opt", "no-new-privileges:true", self.tag)
        try:
            probe = ("fetch('http://127.0.0.1:5173/media').then(async r => console.log(JSON.stringify("
                     "{status: r.status, html: (await r.text()).includes('/assets/')})))"
                     ".catch(() => console.log('{}'))")
            result = {}
            for _ in range(60):
                result = json.loads(docker("exec", name, "node", "-e", probe, check=False).stdout or "{}")
                if result.get("status") == 200:
                    break
                time.sleep(0.5)
            self.assertEqual(result, {"status": 200, "html": True}, "SPA route did not serve the built index")
            uid = docker("exec", name, "sh", "-c", "grep '^Uid:' /proc/1/status").stdout.split()[1:]
            self.assertEqual(set(uid), {"1000"}, "UI server still runs as root")
        finally:
            docker("rm", "-f", name, check=False)


if __name__ == "__main__":
    unittest.main()
