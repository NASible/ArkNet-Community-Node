import importlib.util
import json
import os
import shutil
import subprocess
import tempfile
from pathlib import Path
import unittest

from scripts import harness_scratch


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
                "relay_forwarded_bytes": 4_900_000,
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
                    "relay_forwarded_bytes": 0,
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


class RelayTransportTests(unittest.TestCase):
    """Private relay is the product's only peer transport (Prompt 48D, ARKSEC-004)."""

    def layout(self, root):
        return harness.relay_layout(Path(root))

    def test_core_stack_includes_product_relay_overlay(self):
        files = [str(path) for path in harness.CORE_COMPOSE_FILES]
        self.assertIn("deploy/core/compose.tracker.yaml", files)
        self.assertIn("deploy/core/compose.relay.yaml", files)

    def test_core_env_supplies_tracker_relay_verify_keys_and_relay_secrets(self):
        with tempfile.TemporaryDirectory() as root:
            layout = self.layout(root)
            tracker_public = Path(root) / "tracker-public.key"
            env = harness.relay_core_env(layout, tracker_public, relay_port=41001, control_port=41002)
            self.assertEqual(env["ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE"], str(layout["endpoint_public"]))
            self.assertEqual(env["ARKNET_RELAY_GRANT_VERIFY_KEY_FILE"], str(tracker_public))
            self.assertEqual(env["ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE"], str(layout["endpoint_private"]))
            self.assertEqual(env["ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE"], str(layout["revocation_public"]))
            self.assertEqual(env["ARKNET_CORE_RELAY_CONFIG_DIR"], str(layout["core_config"]))
            self.assertEqual(env["ARKNET_RELAY_ID"], harness.RELAY_ID)
            # Never the developer defaults (7443/7080) that a dev stack may hold.
            self.assertEqual((env["ARKNET_RELAY_PORT"], env["ARKNET_RELAY_CONTROL_PORT"]), ("41001", "41002"))
            for name in ("ARKNET_RELAY_TLS_CERT_FILE", "ARKNET_RELAY_TLS_KEY_FILE"):
                self.assertTrue(env[name].startswith(str(layout["relay_secrets"])))
            # Core's mounted relay directory never holds a relay-side private key.
            for private in (layout["endpoint_private"], layout["tls_key"]):
                self.assertNotIn(str(layout["core_config"]), str(private))

    def test_relays_document_points_nodes_at_the_relay_and_core_at_its_revocation_key(self):
        document = harness.relays_document("ab" * 32)
        self.assertEqual(document, [{
            "id": harness.RELAY_ID, "address": "arknet-relay:7443", "spki_sha256": "ab" * 32,
            "control_url": "http://arknet-relay:7080",
            "revocation_key_file": "/run/config/arknet-relays/revocation/tracker-private.key"}])

    def test_node_env_enables_relay_egress_and_signed_relay_list_only(self):
        env = harness.relay_node_env("PUBLICKEY=")
        self.assertEqual(env, {"ARKNET_RELAY_LIST_VERIFY_KEY": "PUBLICKEY=",
                               "ARKNET_TRANSFER_RELAY_EGRESS": "arknet-relay:7443"})

    def test_node_transfer_networks_have_no_path_to_the_other_node(self):
        images = {"agent": "agent-image", "transfer": "transfer-image"}
        documents = {}
        for label in ("a", "b"):
            cfg = {"jelly_config": Path("/tmp/jc-" + label), "jelly_cache": Path("/tmp/jx-" + label)}
            documents[label] = harness.node_override_document(images, cfg, "p43f-unit-transport-" + label)
        for label, document in documents.items():
            transfer = document["services"]["arknet-transfer"]
            # Exactly the local control network plus this Node's own isolated network.
            self.assertEqual(set(transfer["networks"]), {"transfer-control", "acceptance"})
            self.assertEqual(document["networks"]["acceptance"], {"external": True, "name": "p43f-unit-transport-" + label})
            other = "p43f-unit-transport-" + ("b" if label == "a" else "a")
            self.assertNotIn(other, json.dumps(document))
            rendered = harness.relay_acceptance.compose_override(document)
            self.assertIn("networks: !override", rendered)  # no merged-in direct egress network

    def test_harness_no_longer_bridges_node_a_into_node_b_network(self):
        source = SCRIPT.read_text()
        for forbidden in ('"connect", network_b, transfer_container("a")', '"restart", transfer_container("a")'):
            self.assertFalse(forbidden in source, forbidden)
        for required in ('"--alias", "arknet-relay"', "relay_core_env(", "relay_node_env(",
                         "assert_disjoint_transfer_networks(", "relay_tx_bytes(", '"relay_forwarded_bytes"'):
            self.assertTrue(required in source, required)

    def evidence(self, **changes):
        values = {"object_size": 10, "core_only_egress": 10, "core_egress": 9, "node_a_upload": 5,
                  "node_b_completed": 10, "node_b_elapsed_seconds": 1.0, "node_b_bytes_when_a_stopped": 5,
                  "core_egress_when_a_stopped": 1, "node_a_tx_while_peer_serving_off": 0,
                  "node_a_tx_after_peer_serving_disabled": 0, "control_traffic_limit": 1024,
                  "relay_forwarded_bytes": 5}
        values.update(changes)
        return values

    def test_result_records_relay_transport_and_relay_forwarded_bytes(self):
        result = harness.result_document(run_id="unit", object_id="media_" + "1" * 64, object_sha256="2" * 64,
                                         evidence_values=self.evidence())
        self.assertEqual(result["measurements"]["relay_forwarded_bytes"], 5)
        self.assertEqual(result["proof"]["peer_transport"], "private relay; no Node-to-Node network path")

    def test_result_rejects_peer_payload_that_bypassed_the_relay(self):
        with self.assertRaisesRegex(AssertionError, "relay forwarded no peer payload"):
            harness.result_document(run_id="unit", object_id="media_" + "1" * 64, object_sha256="2" * 64,
                                    evidence_values=self.evidence(relay_forwarded_bytes=0))

    def test_disjoint_transfer_networks_rejects_shared_network(self):
        harness.assert_disjoint_transfer_networks({"x-transfer-control", "net-a"}, {"y-transfer-control", "net-b"})
        with self.assertRaisesRegex(AssertionError, "direct Node-to-Node network path"):
            harness.assert_disjoint_transfer_networks({"net-a", "net-b"}, {"net-b"})

    @unittest.skipUnless(shutil.which("docker"), "docker CLI not installed")
    def test_core_compose_interpolates_with_generated_relay_env(self):
        """Reproduces the gate failure: tracker requires relay endpoint verify keys."""
        with tempfile.TemporaryDirectory(dir=harness_scratch.scratch_root()) as root:
            layout = self.layout(root)
            for directory in (layout["core_config"], layout["relay_secrets"], layout["revocation_public"].parent,
                              layout["endpoint_private"].parent, layout["list_public"].parent):
                directory.mkdir(parents=True, exist_ok=True)
            for path in (layout["endpoint_public"], layout["endpoint_private"], layout["revocation_public"],
                         layout["tls_cert"], layout["tls_key"], Path(root) / "tracker-public.key",
                         Path(root) / "tracker-private.key"):
                path.write_text("x\n")
            env = {k: v for k, v in os.environ.items() if not k.startswith(("ARKNET_", "POSTGRES_"))}
            env.update(POSTGRES_PASSWORD="unit", ARKNET_TRACKER_ANNOUNCE_URL="http://arknet-tracker:6969",
                       ARKNET_CORE_WEB_SEED_ORIGIN="http://arknet-core:8088", ARKNET_CORE_WEB_SEED_SOURCE=root,
                       ARKNET_TRACKER_SIGNING_KEY_FILE=str(Path(root) / "tracker-private.key"),
                       ARKNET_TRACKER_VERIFY_KEY_FILE=str(Path(root) / "tracker-public.key"))
            env.update(harness.relay_core_env(layout, Path(root) / "tracker-public.key", relay_port=41001, control_port=41002))
            override = Path(root) / "core-override.yaml"
            override.write_text(harness.relay_acceptance.compose_override(harness.core_override_document(
                {"core": "c-image", "tracker": "t-image", "relay": "r-image"})))
            command = ["docker", "compose", "-p", "arknet-unit-config"]
            for path in harness.CORE_COMPOSE_FILES:
                command += ["-f", str(harness.ROOT / path)]
            command += ["-f", str(override), "config", "--format", "json"]
            result = subprocess.run(command, cwd=harness.ROOT / "deploy/core", env=env, capture_output=True,
                                    text=True, timeout=60, check=False)
            self.assertEqual(result.returncode, 0, result.stderr[-400:])
            services = json.loads(result.stdout)["services"]
            self.assertEqual(services["arknet-relay"]["image"], "r-image")
            self.assertEqual(set(services["arknet-core"]["networks"]), {"core", "relay-edge"})
            sources = {v["source"] for v in services["arknet-tracker"]["volumes"]}
            self.assertIn(str(layout["endpoint_public"]), sources)


if __name__ == "__main__":
    unittest.main()
