"""Egress policy tests: run with host Python; no firewall is changed."""
import importlib.util
import socket
import unittest
from pathlib import Path
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("transfer_egress", Path(__file__).with_name("entrypoint.py"))
policy = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(policy)


class EgressPolicyTests(unittest.TestCase):
    def test_only_configured_tcp_destinations_are_allowed(self):
        env = {"ARKNET_TRANSFER_TRACKER_ORIGIN": "https://tracker.test",
               "ARKNET_TRANSFER_WEB_SEED_ORIGIN": "http://seed.test:8080",
               "ARKNET_TRANSFER_RELAY_EGRESS": "relay.test:7443"}
        addresses = {"tracker.test": "192.0.2.10", "seed.test": "192.0.2.11", "relay.test": "192.0.2.12"}
        def resolve(host, port, **kwargs):
            return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", (addresses[host], port))]
        with patch.object(socket, "getaddrinfo", side_effect=resolve):
            allowed = policy.destinations(env)
        self.assertEqual(allowed, {("192.0.2.10", 443), ("192.0.2.11", 8080), ("192.0.2.12", 7443)})
        rules = policy.firewall_rules(allowed, socket.AF_INET)
        self.assertIn(":OUTPUT DROP", rules)
        self.assertIn(":INPUT DROP", rules)
        self.assertIn("-A INPUT -i lo -j ACCEPT", rules)
        self.assertIn("-A INPUT -p tcp --dport 8084 -j ACCEPT", rules)
        self.assertIn("-A OUTPUT -d 192.0.2.12 -p tcp --dport 7443 -j ACCEPT", rules)
        self.assertNotIn("--dport 6881", rules)
        self.assertNotIn("0.0.0.0/0", rules)
        self.assertNotIn("-A OUTPUT -p tcp -j ACCEPT", rules)

    def test_ipv6_has_the_same_default_deny_boundary(self):
        rules = policy.firewall_rules({("2001:db8::1", 443), ("192.0.2.1", 7443)}, socket.AF_INET6)
        self.assertIn(":OUTPUT DROP", rules)
        self.assertIn("-d 2001:db8::1 -p tcp --dport 443", rules)
        self.assertNotIn("192.0.2.1", rules)

    def test_unsafe_or_unbounded_relay_configuration_fails(self):
        base = {"ARKNET_TRANSFER_TRACKER_ORIGIN": "https://tracker.test", "ARKNET_TRANSFER_WEB_SEED_ORIGIN": "https://seed.test"}
        for invalid in ("relay.test", "relay.test:0", "relay.test:65536", "user@relay.test:7443", "relay.test:7443/path", ",", "relay.test:7443," , ",".join(["relay.test:7443"] * 17)):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                policy.configured_targets({**base, "ARKNET_TRANSFER_RELAY_EGRESS": invalid})
        for origin in ("https://u:p@tracker.test", "https://tracker.test/path", "ftp://tracker.test"):
            with self.subTest(origin=origin), self.assertRaises(ValueError):
                policy.configured_targets({**base, "ARKNET_TRANSFER_TRACKER_ORIGIN": origin})

    def test_no_relay_configuration_means_no_peer_egress(self):
        targets = policy.configured_targets({"ARKNET_TRANSFER_TRACKER_ORIGIN": "https://tracker.test", "ARKNET_TRANSFER_WEB_SEED_ORIGIN": "https://seed.test"})
        self.assertEqual(targets, {("tracker.test", 443), ("seed.test", 443)})


if __name__ == "__main__":
    unittest.main()
