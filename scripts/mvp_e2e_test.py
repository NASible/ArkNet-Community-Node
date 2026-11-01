import runpy
from pathlib import Path
import unittest

PATH = Path(__file__).with_name('check-mvp-e2e.py')


class MVPHarnessTests(unittest.TestCase):
    def test_enrollment_checks_pinned_keys_in_actual_schema(self):
        source = PATH.read_text()
        self.assertIn('SELECT count(DISTINCT enrollment_public_key) FROM nodes', source)
        self.assertNotIn('FROM node_credentials', source)

    def test_transfer_refresh_cadence_leaves_time_for_payload_progress(self):
        source = PATH.read_text()
        self.assertTrue('ARKNET_TRANSFER_RECONCILE_INTERVAL="30s"' in source)

    def test_swarm_waits_for_product_seed_and_bounds_core_payload(self):
        source = PATH.read_text()
        self.assertIn('A product seed SEEDING', source)
        self.assertIn('assert 0 <= before_disconnect < video["size"]', source)
        self.assertIn('node-agent seed reconciler -> transfer service -> libtorrent worker', source)
        self.assertNotIn('NODE_A_SEEDER', source)

    def test_item_13_covers_the_core_bound_login_model(self):
        # Prompt 46B acceptance: online, offline in window, rejected after it,
        # replay, cross-Node, and the malicious-Node recording harness.
        source = PATH.read_text()
        for needle in ('global_login.login(', 'global_login.RecordingRelay(', 'offline_login_rejected',
                       'def after_window', 'OFFLINE_WINDOW_SECONDS', 'compose.node-login.yaml',
                       '"/api/v1/global-users/sessions", "POST"'):
            self.assertIn(needle, source)
        self.assertNotIn('"password": global_password}, (200, 201))', source)
        self.assertLess(source.index('def offline_auth'), source.index('def offline_playback'))
        self.assertLess(source.index('def offline_playback'), source.index('def after_window'))

    def test_diagnostics_bundle_is_written_before_cleanup_on_any_failure(self):
        # Item failures are recorded without raising; diagnostics must not depend on an exception.
        source = PATH.read_text()
        cleanup = source.index('node_compose(label, "down"')
        call = source.index('write_diagnostics(failure)')
        self.assertLess(call, cleanup)
        self.assertIn('if failure is not None or any(v["status"] == "FAIL" for v in results.items.values()):', source)
        for section in ('-transfers', '-seeds', '-worker-events', '-logs', '"results"'):
            self.assertIn(section, source)
        self.assertIn('e2e_diagnostics.write_bundle(directory, sections, known_secrets, temporary)', source)

    def test_swarm_check_writes_bundle_before_cleanup(self):
        source = PATH.with_name('check-multi-node-swarm-stack.py').read_text()
        self.assertLess(source.index('e2e_diagnostics.write_bundle('), source.index('docker", "rm", "-f", meter_name'))
        self.assertIn('"meter-log"', source)


if __name__ == '__main__':
    unittest.main()
