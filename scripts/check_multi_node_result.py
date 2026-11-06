"""Sanitized result document for the product-path swarm proof."""
from scripts.multi_node_swarm_evidence import Evidence


def result_document(*, run_id, object_id, object_sha256, evidence_values):
    evidence = Evidence(**evidence_values)
    evidence.validate()
    measurements = dict(evidence_values)
    measurements["node_b_average_bytes_per_second"] = evidence.node_b_average_bytes_per_second
    measurements["core_egress_after_node_a_stopped"] = evidence.core_egress_after_node_a_stopped
    return {
        "schema": "arknet.prompt43f.product-swarm-proof.v1",
        "run_id": run_id,
        "object": {"id": object_id, "sha256": object_sha256, "playable_mp4": True},
        "measurements": measurements,
        "proof": {
            "private_v2_only": True,
            "multi_piece": True,
            "dht_pex_lsd_disabled": True,
            "node_a_enabled_via_local_api": True,
            "core_eligibility_observed": True,
            "node_a_seed_path": "node-agent seed reconciler -> transfer service -> libtorrent worker",
            "peer_transport": "private relay; no Node-to-Node network path",
            "peer_serving_off_proved": True,
            "peer_serving_disable_stopped_upload": True,
            "core_meter_scope": "successful Core web-seed response-body bytes only; torrent metadata excluded",
            "node_b_exact_sha256_match": True,
        },
        "acceptance": {"evidence_validate_passed": True, "product_path": True},
    }
