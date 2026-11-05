"""Real-binding policy tests for relay-only libtorrent networking."""
import socket
import tempfile
import unittest
from pathlib import Path
import worker
import libtorrent as lt


class RelayWorkerPolicyTests(unittest.TestCase):
    def test_peers_are_tcp_socks5_only_and_tracker_stays_unproxied(self):
        settings = worker.session_settings("127.0.0.1:6881", 0, False, proxy_port=19080)
        self.assertEqual(settings["proxy_type"], int(lt.proxy_type_t.socks5))
        self.assertEqual(settings["proxy_hostname"], "127.0.0.1")
        self.assertEqual(settings["proxy_port"], 19080)
        self.assertTrue(settings["proxy_peer_connections"])
        self.assertTrue(settings["proxy_hostnames"])
        self.assertFalse(settings["proxy_tracker_connections"])
        self.assertFalse(settings["enable_outgoing_utp"])
        self.assertFalse(settings["enable_incoming_utp"])
        self.assertEqual(settings["out_enc_policy"], int(lt.enc_policy.disabled))
        self.assertEqual(settings["in_enc_policy"], int(lt.enc_policy.disabled))
        self.assertTrue(settings["allow_multiple_connections_per_ip"])
        actual = lt.session(settings).get_settings()
        for key in settings:
            if key in actual:
                self.assertEqual(actual[key], settings[key], key)

    def test_default_proxy_is_fail_closed_not_direct(self):
        settings = worker.session_settings("0.0.0.0:6881", 0)
        self.assertEqual(settings["proxy_type"], int(lt.proxy_type_t.socks5))
        self.assertTrue(settings["proxy_peer_connections"])
        self.assertEqual(settings["proxy_hostname"], "127.0.0.1")
        self.assertGreater(settings["proxy_port"], 0)
        self.assertEqual(settings["listen_interfaces"], "127.0.0.1:6881")


class RelayWorkerListenerTests(unittest.TestCase):
    """libtorrent opens no TCP listener at all once a SOCKS5 proxy is set, so
    seeds live in a second, unproxied session that listens on loopback only."""

    def manager(self):
        root = tempfile.TemporaryDirectory()
        self.addCleanup(root.cleanup)
        base = Path(root.name)
        (base / "media").mkdir()
        value = worker.Manager("http://127.0.0.1:9", str(base / "staging"), str(base / "state"),
                               "127.0.0.1:0", 0, "", False, str(base / "media"))
        self.addCleanup(value.close)
        return value

    def test_seed_session_accepts_connector_dials_on_loopback(self):
        manager = self.manager()
        port = manager.connector.listen_port
        self.assertGreater(port, 0)
        with socket.create_connection(("127.0.0.1", port), timeout=2):
            pass
        self.assertEqual(manager.seed_session.listen_port(), port)
        seed = manager.seed_session.get_settings()
        self.assertEqual(seed["proxy_type"], int(lt.proxy_type_t.none))
        self.assertTrue(seed["listen_interfaces"].startswith("127.0.0.1:"))
        self.assertFalse(seed["enable_outgoing_utp"] or seed["enable_incoming_utp"])
        self.assertEqual(seed["out_enc_policy"], int(lt.enc_policy.disabled))
        self.assertTrue(seed["allow_multiple_connections_per_ip"])

    def test_download_session_stays_socks5_only(self):
        manager = self.manager()
        download = manager.session.get_settings()
        self.assertEqual(download["proxy_type"], int(lt.proxy_type_t.socks5))
        self.assertEqual(download["proxy_port"], manager.connector.proxy_port)
        self.assertTrue(download["proxy_peer_connections"])
        self.assertIsNot(manager.session, manager.seed_session)


if __name__ == "__main__":
    unittest.main()
