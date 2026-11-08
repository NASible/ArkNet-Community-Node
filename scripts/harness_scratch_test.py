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
