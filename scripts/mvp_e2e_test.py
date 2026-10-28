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


if __name__ == '__main__':
    unittest.main()
