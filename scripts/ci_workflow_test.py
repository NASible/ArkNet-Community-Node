"""Prompt 51B: guard rails for the public CI workflow (text checks; no YAML parser in stdlib)."""
import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
WORKFLOWS = sorted((ROOT / ".github" / "workflows").glob("*.y*ml"))


class WorkflowTests(unittest.TestCase):
    def test_workflows_exist(self):
        self.assertTrue(WORKFLOWS)

    def test_actions_are_pinned_by_commit(self):
        for path in WORKFLOWS:
            for line in re.findall(r"(?m)^\s*-?\s*uses:\s*(\S+)", path.read_text()):
                with self.subTest(workflow=path.name, uses=line):
                    self.assertRegex(line, r"^[\w.-]+/[\w./-]+@[0-9a-f]{40}$")

    def test_no_secrets_and_read_only_token(self):
        for path in WORKFLOWS:
            text = path.read_text()
            with self.subTest(workflow=path.name):
                self.assertNotRegex(text, r"\$\{\{[^}]*\bsecrets\.", "the gate needs no repository secrets")
                self.assertNotIn("pull_request_target", text, "never run fork code with a privileged token")
                self.assertRegex(text, r"(?m)^permissions:\n  contents: read\n")
                self.assertNotRegex(text, r"(?m)^\s+\w[\w-]*: write\b")
                self.assertEqual(text.count("persist-credentials: false"), text.count("actions/checkout@"))

    def test_every_log_upload_is_gated_by_the_secret_scan(self):
        for path in WORKFLOWS:
            text = path.read_text()
            uploads = text.count("actions/upload-artifact@")
            with self.subTest(workflow=path.name):
                self.assertEqual(len(re.findall(r"id: scan\n\s+if: always\(\)\n\s+run: python3 scripts/check-log-secrets.py", text)),
                                 uploads)
                self.assertEqual(text.count("if: always() && steps.scan.outcome != 'failure'"), uploads)

    def test_gate_modes(self):
        text = (ROOT / ".github" / "workflows" / "gate.yml").read_text()
        self.assertIn("run: scripts/check-all.sh --logs", text)
        self.assertIn("run: scripts/check-all.sh --e2e --vm --logs", text)
        for trigger in ("push:", "pull_request:", "schedule:", "workflow_dispatch:", 'tags: ["v*"]'):
            self.assertIn(trigger, text)


if __name__ == "__main__":
    unittest.main()
