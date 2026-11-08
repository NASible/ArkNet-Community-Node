"""Prompt 51B acceptance: this branch must show a failed CI run. Never merge it."""
import unittest


class DeliberateFailure(unittest.TestCase):
    def test_ci_reports_this_failure(self):
        self.fail("deliberate failure to prove CI reports the failing step (Prompt 51B)")
