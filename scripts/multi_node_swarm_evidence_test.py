import unittest

from scripts.multi_node_swarm_evidence import Evidence, EvidenceError


class EvidenceTests(unittest.TestCase):
    def valid(self, **changes):
        values = {
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
        }
        values.update(changes)
        return Evidence(**values)

    def test_accepts_peer_contribution_and_core_fallback_after_peer_loss(self):
        evidence = self.valid()

        evidence.validate()

        self.assertAlmostEqual(evidence.node_b_average_bytes_per_second, 16_777_216 / 24.0)
        self.assertGreater(evidence.core_egress_after_node_a_stopped, 0)

    def test_rejects_run_without_node_a_payload(self):
        with self.assertRaisesRegex(EvidenceError, "Node A contributed no payload"):
            self.valid(node_a_upload=0).validate()

    def test_rejects_core_egress_not_lower_than_core_only(self):
        with self.assertRaisesRegex(EvidenceError, "Core egress was not reduced"):
            self.valid(core_egress=16_777_216).validate()

    def test_rejects_incomplete_node_b(self):
        with self.assertRaisesRegex(EvidenceError, "Node B did not complete"):
            self.valid(node_b_completed=16_000_000).validate()

    def test_rejects_peer_stopped_only_after_completion(self):
        with self.assertRaisesRegex(EvidenceError, "Node A was not removed mid-transfer"):
            self.valid(node_b_bytes_when_a_stopped=16_777_216).validate()

    def test_rejects_run_without_core_fallback_after_peer_loss(self):
        with self.assertRaisesRegex(EvidenceError, "Core supplied no payload after Node A disappeared"):
            self.valid(core_egress=1_000_000, core_egress_when_a_stopped=1_000_000).validate()

    def test_rejects_unmeasured_source_after_node_a_disappeared(self):
        # Shape of a real bogus run: a stray seed finished Node B with almost no Core egress.
        with self.assertRaisesRegex(EvidenceError, "does not cover the object; unmeasured source"):
            self.valid(core_egress=131_072, core_egress_when_a_stopped=0,
                       node_a_upload=5_518_920, node_b_bytes_when_a_stopped=4_456_448).validate()

    def test_accepts_bytes_in_flight_from_node_a_when_it_stopped(self):
        # Node B's verified progress lags bytes Node A already delivered: here
        # 6 MB were sent but only 4 MB verified, so Core needs to send only
        # object_size - 6 MB after the stop. That is fully accounted, not a leak.
        evidence = self.valid(node_a_upload=6_000_000, node_b_bytes_when_a_stopped=4_000_000,
                              core_egress_when_a_stopped=0, core_egress=10_777_216)
        evidence.validate()

    def test_rejects_sources_that_do_not_cover_object(self):
        with self.assertRaisesRegex(EvidenceError, "does not cover the object"):
            self.valid(node_a_upload=50_000, node_b_bytes_when_a_stopped=100_000,
                       core_egress=16_677_216, core_egress_when_a_stopped=0,
                       object_size=16_777_216, node_b_completed=16_777_216,
                       core_only_egress=17_000_000).validate()

    def test_rejects_peer_serving_off_network_payload(self):
        with self.assertRaisesRegex(EvidenceError, "Peer Serving OFF"):
            self.valid(node_a_tx_while_peer_serving_off=131_073).validate()

    def test_rejects_upload_continuing_after_disable(self):
        with self.assertRaisesRegex(EvidenceError, "continued after Peer Serving was disabled"):
            self.valid(node_a_tx_after_peer_serving_disabled=131_073).validate()

    def test_rejects_invalid_control_traffic_limit(self):
        with self.assertRaisesRegex(EvidenceError, "control traffic limit"):
            self.valid(control_traffic_limit=0).validate()

    def test_result_document_declares_product_path_without_adapter(self):
        from scripts.check_multi_node_result import result_document
        values = self.valid().__dict__
        result = result_document(run_id="run", object_id="media_" + "a" * 64,
                                 object_sha256="b" * 64, evidence_values=values)
        proof = result["proof"]
        self.assertEqual(proof["node_a_seed_path"], "node-agent seed reconciler -> transfer service -> libtorrent worker")
        self.assertTrue(proof["peer_serving_off_proved"])
        self.assertTrue(proof["peer_serving_disable_stopped_upload"])
        self.assertNotIn("adapter", " ".join(map(str, proof.values())).lower())

    def test_rejects_invalid_measurements(self):
        with self.assertRaisesRegex(EvidenceError, "positive"):
            self.valid(node_b_elapsed_seconds=0).validate()
        with self.assertRaisesRegex(EvidenceError, "non-negative"):
            self.valid(core_egress=-1).validate()


if __name__ == "__main__":
    unittest.main()
