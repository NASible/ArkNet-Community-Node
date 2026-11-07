"""Torrent fixtures must describe the bytes the host wrote, not a stale view.

Docker Desktop/Colima shared folders can briefly serve a stale file size to a
container right after the host appends to it. The Prompt 50 frozen-tree E2E hit
this: the helper hashed a 1,753,434-byte prefix of a padded 96 MiB fixture,
reported a second (fresh) stat as the size, and the harness accepted a torrent
whose payload did not match Core's catalog size. The worker then correctly
rejected the metadata and item 16 failed for a harness reason.
"""
from pathlib import Path
import ast
import runpy
import unittest

ROOT = Path(__file__).resolve().parent.parent


class MakeTorrentFixtureTest(unittest.TestCase):
    def setUp(self):
        self.source = runpy.run_path(str(ROOT / "scripts/check-core-to-node-transfer-stack.py"),
                                     run_name="fixture_test")["MAKE_TORRENT"]
        ast.parse(self.source)

    def test_reported_size_comes_from_the_generated_torrent(self):
        # A second stat() can disagree with the size that was hashed.
        self.assertNotIn('"size": source.stat().st_size', self.source)
        self.assertIn("info.files().total_size()", self.source)

    def test_helper_waits_for_the_expected_host_size_before_hashing(self):
        self.assertIn("sys.argv[3]", self.source)
        add = self.source.index("files.add_file(")
        wait = self.source.index("expected")
        self.assertLess(wait, add, "the expected size must be checked before the torrent is built")

    def test_e2e_passes_the_host_size_to_the_helper(self):
        e2e = (ROOT / "scripts/check-mvp-e2e.py").read_text()
        call = e2e[e2e.index('"/make-torrent.py", "/work", name'):][:200]
        self.assertIn("str(path.stat().st_size)", call)


if __name__ == "__main__":
    unittest.main()
