"""Harness scratch space must work on any machine, not only the maintainer's Mac."""
import os
import re
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from scripts import harness_scratch

ROOT = Path(__file__).resolve().parent.parent


class ScratchRootTests(unittest.TestCase):
    def test_explicit_override_wins_and_is_created(self):
        with tempfile.TemporaryDirectory() as base:
            wanted = Path(base) / "a" / "b"
            with mock.patch.dict(os.environ, {"ARKNET_SCRATCH": str(wanted)}):
                self.assertEqual(harness_scratch.scratch_root(), wanted)
            self.assertTrue(wanted.is_dir())

    def test_without_hermes_scratch_falls_back_to_system_temp(self):
        with tempfile.TemporaryDirectory() as home:
            env = {k: v for k, v in os.environ.items() if k != "ARKNET_SCRATCH"}
            with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(Path, "home", return_value=Path(home)):
                self.assertEqual(harness_scratch.scratch_root(), Path(tempfile.gettempdir()))
                self.assertFalse((Path(home) / ".hermes").exists(), "never create the maintainer's scratch dir")

    def test_existing_hermes_scratch_is_used(self):
        with tempfile.TemporaryDirectory() as home:
            scratch = Path(home) / ".hermes" / "cache" / "scratch"
            scratch.mkdir(parents=True)
            env = {k: v for k, v in os.environ.items() if k != "ARKNET_SCRATCH"}
            with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(Path, "home", return_value=Path(home)):
                self.assertEqual(harness_scratch.scratch_root(), scratch)


class RemoveTreeTests(unittest.TestCase):
    def test_plain_tree_is_removed_without_docker(self):
        with tempfile.TemporaryDirectory() as base:
            tree = Path(base) / "t"
            (tree / "a").mkdir(parents=True)
            (tree / "a" / "f").write_text("x")
            with mock.patch.object(harness_scratch.subprocess, "run") as run:
                harness_scratch.remove_tree(tree)
            self.assertFalse(tree.exists())
            run.assert_not_called()

    def test_missing_tree_is_fine(self):
        harness_scratch.remove_tree(Path("/nonexistent/arknet-tree"))

    def test_undeletable_files_are_removed_by_a_pinned_root_container(self):
        with tempfile.TemporaryDirectory() as base:
            tree = Path(base) / "t"
            tree.mkdir()
            calls = []
            real_rmtree = harness_scratch.shutil.rmtree

            def fake_rmtree(path, *args, **kwargs):
                if not calls:
                    raise PermissionError("root-owned")
                real_rmtree(path)

            def fake_run(command, **kwargs):
                calls.append(command)
                return mock.Mock(returncode=0)

            with mock.patch.object(harness_scratch.shutil, "rmtree", fake_rmtree), \
                    mock.patch.object(harness_scratch.subprocess, "run", fake_run):
                harness_scratch.remove_tree(tree)
            self.assertFalse(tree.exists())
            (command,) = calls
            self.assertEqual(command[:3], ["docker", "run", "--rm"])
            self.assertIn("--network=none", command)
            self.assertIn(f"{tree.resolve()}:/w", command)
            image = next(c for c in command if c.startswith("postgres:"))
            self.assertRegex(image, r"@sha256:[0-9a-f]{64}$")
            self.assertIn(image, (ROOT / "deploy/core/compose.yaml").read_text())

    def test_failure_is_raised_when_the_container_cannot_help(self):
        with tempfile.TemporaryDirectory() as base:
            tree = Path(base) / "t"
            tree.mkdir()
            with mock.patch.object(harness_scratch.shutil, "rmtree", side_effect=PermissionError("root-owned")), \
                    mock.patch.object(harness_scratch.subprocess, "run", return_value=mock.Mock(returncode=1)):
                with self.assertRaises(PermissionError):
                    harness_scratch.remove_tree(tree)


class HostForContainersTests(unittest.TestCase):
    def test_macos_uses_loopback_and_the_desktop_host_name(self):
        with mock.patch.object(harness_scratch.platform, "system", return_value="Darwin"), \
                mock.patch.object(harness_scratch.subprocess, "run") as run:
            self.assertEqual(harness_scratch.host_for_containers(), ("127.0.0.1", "host.docker.internal"))
        run.assert_not_called()

    def test_linux_binds_and_dials_the_docker_bridge_gateway(self):
        result = mock.Mock(returncode=0, stdout="172.17.0.1\n")
        with mock.patch.object(harness_scratch.platform, "system", return_value="Linux"), \
                mock.patch.object(harness_scratch.subprocess, "run", return_value=result) as run:
            self.assertEqual(harness_scratch.host_for_containers(), ("172.17.0.1", "172.17.0.1"))
        self.assertEqual(run.call_args.args[0][:4], ["docker", "network", "inspect", "bridge"])

    def test_linux_without_a_bridge_gateway_fails_loudly(self):
        for result in (mock.Mock(returncode=1, stdout=""), mock.Mock(returncode=0, stdout="not-an-ip\n"),
                       mock.Mock(returncode=0, stdout="0.0.0.0\n")):
            with self.subTest(stdout=result.stdout), \
                    mock.patch.object(harness_scratch.platform, "system", return_value="Linux"), \
                    mock.patch.object(harness_scratch.subprocess, "run", return_value=result):
                with self.assertRaises(RuntimeError):
                    harness_scratch.host_for_containers()


class AsOperatorTests(unittest.TestCase):
    def test_macos_and_root_need_no_prefix(self):
        with mock.patch.object(harness_scratch.platform, "system", return_value="Darwin"):
            self.assertEqual(harness_scratch.as_operator({"PATH": "/x"}), [])
        with mock.patch.object(harness_scratch.platform, "system", return_value="Linux"), \
                mock.patch.object(harness_scratch.os, "geteuid", return_value=0):
            self.assertEqual(harness_scratch.as_operator({"PATH": "/x"}), [])

    def test_linux_user_uses_non_interactive_sudo_keeping_only_path(self):
        with mock.patch.object(harness_scratch.platform, "system", return_value="Linux"), \
                mock.patch.object(harness_scratch.os, "geteuid", return_value=1000):
            self.assertEqual(harness_scratch.as_operator({"PATH": "/opt/ssl/bin:/usr/bin", "SECRET": "x"}),
                             ["sudo", "-n", "env", "PATH=/opt/ssl/bin:/usr/bin"])


class NoHardCodedScratchTests(unittest.TestCase):
    def test_gate_code_does_not_hard_code_the_hermes_scratch_dir(self):
        allowed = {Path("scripts/harness_scratch.py"), Path("scripts/harness_scratch_test.py")}
        offenders = []
        for top in ("scripts", "transfer", "core", "node-agent", "tracker", "relay", "deploy/node"):
            for path in (ROOT / top).rglob("*"):
                rel = path.relative_to(ROOT)
                if path.suffix not in {".py", ".sh", ".go"} or rel in allowed or "spikes" in rel.parts:
                    continue
                if re.search(r"\.hermes", path.read_text(errors="replace")):
                    offenders.append(str(rel))
        self.assertEqual(offenders, [], "use harness_scratch.scratch_root()")


if __name__ == "__main__":
    unittest.main()
