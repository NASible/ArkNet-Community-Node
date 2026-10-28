import importlib.util
import json
from pathlib import Path
import unittest


SCRIPT = Path(__file__).with_name("check-multi-node-swarm-stack.py")
spec = importlib.util.spec_from_file_location("check_multi_node_swarm_stack", SCRIPT)
harness = importlib.util.module_from_spec(spec)
spec.loader.exec_module(harness)


class MultiNodeSwarmHarnessTests(unittest.TestCase):
    def test_result_document_is_complete_and_contains_no_capability_urls(self):
        result = harness.result_document(
            run_id="arknet-p43f-test-unit",
            object_id="media_" + "1" * 64,
            object_sha256="2" * 64,
            evidence_values={
                "object_size": 16_777_216,
                "core_only_egress": 16_777_216,
                "core_egress": 12_000_000,
                "node_a_upload": 4_777_216,
                "node_b_completed": 16_777_216,
                "node_b_elapsed_seconds": 24.0,
                "node_b_bytes_when_a_stopped": 6_000_000,
                "core_egress_when_a_stopped": 1_000_000,
                "node_a_tx_while_peer_serving_off": 8_192,
                "node_a_tx_after_peer_serving_disabled": 4_096,
                "control_traffic_limit": 131_072,
            },
        )

        self.assertEqual(result["schema"], "arknet.prompt43f.product-swarm-proof.v1")
        self.assertEqual(result["measurements"]["node_b_average_bytes_per_second"], 16_777_216 / 24.0)
        self.assertEqual(result["measurements"]["core_egress_after_node_a_stopped"], 11_000_000)
        self.assertTrue(result["acceptance"]["evidence_validate_passed"])
        self.assertTrue(result["proof"]["private_v2_only"])
        self.assertTrue(result["proof"]["dht_pex_lsd_disabled"])
        encoded = json.dumps(result)
        self.assertNotIn("/announce/", encoded)
        self.assertNotIn("/api/web-seed/", encoded)
        self.assertNotIn("Bearer ", encoded)

    def test_result_document_rejects_invalid_evidence(self):
        with self.assertRaisesRegex(AssertionError, "Node A contributed no payload"):
            harness.result_document(
                run_id="unit", object_id="media_" + "1" * 64, object_sha256="2" * 64,
                evidence_values={
                    "object_size": 10, "core_only_egress": 10, "core_egress": 9,
                    "node_a_upload": 0, "node_b_completed": 10,
                    "node_b_elapsed_seconds": 1.0, "node_b_bytes_when_a_stopped": 5,
                    "core_egress_when_a_stopped": 1,
                    "node_a_tx_while_peer_serving_off": 0,
                    "node_a_tx_after_peer_serving_disabled": 0,
                    "control_traffic_limit": 1024,
                },
            )

    def test_meter_counts_only_successful_web_seed_response_payload(self):
        source = harness.METER_PROXY
        self.assertIn('path.startswith("/api/web-seed/")', source)
        self.assertIn("payload_bytes", source)
        self.assertIn('self.path == "/__reset"', source)
        self.assertIn("next_send", source)
        self.assertLess(source.index("time.sleep(wait)"), source.index("self.wfile.write(chunk)"))
        self.assertNotIn("print(self.path", source)

    def test_harness_uses_product_seed_reconciler_without_adapter(self):
        source = SCRIPT.read_text()
        self.assertIn('Node A product seed reaches SEEDING', source)
        self.assertIn('/api/transfer/v1/seeds', source)
        self.assertIn('/api/v1/settings/peer-serving', source)
        self.assertIn('Peer Serving OFF unseeds Node A', source)
        self.assertNotIn('NODE_A_SEEDER', source)
        self.assertNotIn('node-a-seeder', source)

    def test_core_origin_is_proxied_under_allowed_arknet_core_name_on_isolated_node_networks(self):
        source = SCRIPT.read_text()
        self.assertIn('ARKNET_CORE_WEB_SEED_ORIGIN="http://arknet-core:8088"', source)
        self.assertIn('ARKNET_CORE_URL="http://arknet-core-origin:8080"', source)
        self.assertIn('"UPSTREAM=http://arknet-core-origin:8080"', source)
        self.assertIn('network_a = prefix + "-node-a-transport"', source)
        self.assertIn('network_b = prefix + "-node-b-transport"', source)
        self.assertIn('"--alias", "arknet-core"', source)
        self.assertIn('"--alias", "arknet-tracker"', source)
        self.assertIn('ARKNET_HEARTBEAT_INTERVAL="5s"', source)
        # The Node Agent rejects reconcile intervals outside 5s..5m at startup.
        self.assertIn('ARKNET_TRANSFER_RECONCILE_INTERVAL="5s"', source)

    def test_prompt25_loader_exposes_reused_orchestration_helpers(self):
        definitions = harness.load_prompt25()
        for name in ("run", "request", "request_json", "wait_until", "jellyfin_setup", "KEY_HELPER", "MAKE_TORRENT"):
            self.assertIn(name, definitions)


if __name__ == "__main__":
    unittest.main()
