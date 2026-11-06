"""Acceptance evidence model for the Prompt 27 disposable swarm proof."""
from dataclasses import dataclass


class EvidenceError(AssertionError):
    """Raised when a measured run does not prove the acceptance criteria."""


@dataclass(frozen=True)
class Evidence:
    object_size: int
    core_only_egress: int
    core_egress: int
    node_a_upload: int
    node_b_completed: int
    node_b_elapsed_seconds: float
    node_b_bytes_when_a_stopped: int
    core_egress_when_a_stopped: int
    node_a_tx_while_peer_serving_off: int
    node_a_tx_after_peer_serving_disabled: int
    control_traffic_limit: int
    # Bytes the private relay sent while Node A was seeding. The relay is the
    # only peer transport (no Node-to-Node network path), so Node A's upload
    # must appear here on its way to Node B.
    relay_forwarded_bytes: int

    @property
    def node_b_average_bytes_per_second(self):
        return self.node_b_completed / self.node_b_elapsed_seconds

    @property
    def core_egress_after_node_a_stopped(self):
        return self.core_egress - self.core_egress_when_a_stopped

    def validate(self):
        integer_measurements = (
            self.object_size,
            self.core_only_egress,
            self.core_egress,
            self.node_a_upload,
            self.node_b_completed,
            self.node_b_bytes_when_a_stopped,
            self.core_egress_when_a_stopped,
            self.node_a_tx_while_peer_serving_off,
            self.node_a_tx_after_peer_serving_disabled,
            self.control_traffic_limit,
            self.relay_forwarded_bytes,
        )
        if any(value < 0 for value in integer_measurements):
            raise EvidenceError("measurements must be non-negative")
        if self.object_size <= 0 or self.core_only_egress <= 0 or self.node_b_elapsed_seconds <= 0:
            raise EvidenceError("object size, baseline egress, and elapsed time must be positive")
        if self.control_traffic_limit <= 0:
            raise EvidenceError("control traffic limit must be positive")
        if self.node_a_tx_while_peer_serving_off > self.control_traffic_limit:
            raise EvidenceError("Node A sent payload while Peer Serving OFF")
        if self.node_a_tx_after_peer_serving_disabled > self.control_traffic_limit:
            raise EvidenceError("Node A upload continued after Peer Serving was disabled")
        if self.node_a_upload <= 0:
            raise EvidenceError("Node A contributed no payload")
        if self.relay_forwarded_bytes <= 0:
            raise EvidenceError("relay forwarded no peer payload")
        if self.core_egress >= self.core_only_egress:
            raise EvidenceError("Core egress was not reduced below the Core-only download")
        if self.node_b_completed != self.object_size:
            raise EvidenceError("Node B did not complete the exact object")
        if not 0 < self.node_b_bytes_when_a_stopped < self.object_size:
            raise EvidenceError("Node A was not removed mid-transfer")
        if self.core_egress_after_node_a_stopped <= 0:
            raise EvidenceError("Core supplied no payload after Node A disappeared")
        # Byte accounting: every completed byte must come from a measured source.
        # node_a_upload is Node A's final total after it was stopped, and
        # core_egress is Core's total for Node B's transfer. A shortfall means an
        # unmeasured peer served Node B, which voids the proof.
        #
        # A per-phase check such as "Core egress after the stop >= object size
        # minus Node B's progress at the stop" is unsound: progress counts only
        # verified pieces, polled ~1 s apart, so bytes Node A already delivered
        # look missing. Bounding those in-flight bytes by
        # (node_a_upload + core_egress_when_a_stopped - node_b_bytes_when_a_stopped)
        # reduces that check algebraically to this total-coverage check.
        if self.node_a_upload + self.core_egress < self.object_size:
            raise EvidenceError("Node A upload plus Core egress does not cover the object; unmeasured source")
