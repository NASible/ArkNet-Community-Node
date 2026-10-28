import json
import os
import secrets
import stat
import tempfile
import unittest
from pathlib import Path

from scripts import e2e_diagnostics as diagnostics

OBJECT_ID = "media_" + "c" * 64


class DiagnosticsBundleTests(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.TemporaryDirectory()
        self.target = Path(self.root.name) / "bundle"
        self.secrets = [secrets.token_hex(32), "global-" + secrets.token_urlsafe(18), secrets.token_urlsafe(24)]
        # A grant Core minted during the run: not in the harness's known-secret list.
        self.minted_grant = secrets.token_urlsafe(12) + "." + secrets.token_urlsafe(48)

    def tearDown(self):
        self.root.cleanup()

    def forced_failure_sections(self):
        """What a failing harness run collects: every section deliberately carries secrets."""
        token, password, jelly = self.secrets

        def raises():
            raise RuntimeError("request to http://core/api/admin?token=" + token + " failed")

        error = None
        try:
            raise AssertionError("timed out; last response used password " + password)
        except AssertionError as caught:
            error = caught
        sections = diagnostics.collect({
            "node-a-transfers": lambda: {"transfers": [{"object_id": OBJECT_ID, "state": "DOWNLOADING"}],
                                         "debug_header": "Authorization: Bearer " + token},
            "node-a-worker-events": lambda: (
                '{"event":"transfer_waiting_for_source"}\n'
                "GET /announce/" + self.minted_grant + " HTTP/1.1\n"
                "GET /api/web-seed/v1/media/" + OBJECT_ID + "/" + self.minted_grant + " 206\n"
                "GET /api/torrent/v1/media/" + OBJECT_ID + "/" + self.minted_grant + " 200\n"),
            "jellyfin-log": lambda: 'Authorization: MediaBrowser Token="' + jelly + '" api_key=' + jelly,
            "work-paths": lambda: "bind mount " + str(Path(self.root.name) / "work") + "/tracker-private.key",
            "broken-collector": raises,
        })
        sections["failure"] = diagnostics.failure_text(error)
        return sections

    def test_forced_failure_bundle_contains_no_known_secret_or_grant(self):
        bundle = diagnostics.write_bundle(self.target, self.forced_failure_sections(), self.secrets,
                                          temporary=Path(self.root.name) / "work")
        self.assertEqual(diagnostics.scan(bundle, self.secrets + [self.minted_grant]), 0)
        text = "\n".join(p.read_text() for p in bundle.glob("*.txt"))
        self.assertNotIn(str(Path(self.root.name) / "work"), text)
        self.assertIn("/announce/<grant>", text)
        self.assertIn("/api/web-seed/v1/media/" + OBJECT_ID + "/<grant>", text)
        self.assertIn('"state": "DOWNLOADING"', text, "useful diagnostics must survive redaction")
        self.assertIn("<collector failed: RuntimeError>", text)
        self.assertIn("AssertionError", text)

    def test_bundle_lists_sections_and_is_owner_only(self):
        bundle = diagnostics.write_bundle(self.target, {"a b/c": "x", "status": "ok"}, self.secrets)
        index = json.loads((bundle / "index.json").read_text())
        self.assertEqual(set(index["sections"]), {"a-b-c", "status"})
        self.assertEqual(stat.S_IMODE(os.stat(bundle).st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(os.stat(bundle / "status.txt").st_mode), 0o600)

    def test_bundle_that_still_contains_a_secret_is_deleted(self):
        original = diagnostics.redact
        diagnostics.redact = lambda text, secrets, temporary=None: text  # simulate a redaction bug
        try:
            with self.assertRaisesRegex(RuntimeError, "bundle removed"):
                diagnostics.write_bundle(self.target, {"leak": "value " + self.secrets[0]}, self.secrets)
        finally:
            diagnostics.redact = original
        self.assertFalse(self.target.exists())

    def test_default_directory_follows_results_file(self):
        results = Path(self.root.name) / "run" / "e2e.json"
        self.assertEqual(diagnostics.default_directory("run-1", str(results)),
                         results.parent.resolve() / "run-1-diagnostics")
        self.assertEqual(diagnostics.default_directory("run-1").name, "run-1")


if __name__ == "__main__":
    unittest.main()
