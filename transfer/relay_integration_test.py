"""Opt-in real Go relay/tracker + two libtorrent sessions (no web seed).

Set ARKNET_TEST_RELAY_BINARY and ARKNET_TEST_TRACKER_BINARY to Linux binaries
inside the transfer image. Run as the product UID with --network none.
Only aggregate diagnostics are emitted; ephemeral signing keys stay private.
"""
import base64
import datetime
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import socket
import struct
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch


@unittest.skipUnless(os.environ.get("ARKNET_TEST_RELAY_BINARY") and
                     os.environ.get("ARKNET_TEST_TRACKER_BINARY"),
                     "real relay/tracker binaries not configured")
class RealRelayIntegrationTests(unittest.TestCase):
    def test_private_v2_seed_is_advertised_and_downloaded_via_relay(self):
        import libtorrent as lt
        from cryptography import x509
        from cryptography.hazmat.primitives import hashes, serialization
        from cryptography.hazmat.primitives.asymmetric import ed25519, ec
        from cryptography.x509.oid import NameOID
        from relay_connector import Connector
        from worker import seed_session_settings, session_settings

        root = Path(tempfile.mkdtemp(prefix="relay-integration-",
                                    dir=os.environ.get("ARKNET_TEST_TMPDIR")))
        self.addCleanup(__import__("shutil").rmtree, root)
        def save(name, data):
            path = root / name
            path.write_bytes(data)
            path.chmod(0o600)
            return str(path)
        def keypair(name):
            key = ed25519.Ed25519PrivateKey.generate()
            public = key.public_key().public_bytes(serialization.Encoding.Raw,
                                                   serialization.PublicFormat.Raw)
            seed = key.private_bytes(serialization.Encoding.Raw,
                                     serialization.PrivateFormat.Raw,
                                     serialization.NoEncryption())
            return key, save(name + ".pub", base64.b64encode(public)), save(
                name + ".key", base64.b64encode(seed + public))
        grant_key, grant_pub, _ = keypair("grant")
        _, revoke_pub, _ = keypair("revocation")
        _, endpoint_pub, endpoint_key = keypair("endpoint")
        tlskey = ec.generate_private_key(ec.SECP256R1())
        name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "probe")])
        now = datetime.datetime.now(datetime.timezone.utc)
        cert = (x509.CertificateBuilder().subject_name(name).issuer_name(name)
                .public_key(tlskey.public_key()).serial_number(x509.random_serial_number())
                .not_valid_before(now - datetime.timedelta(minutes=1))
                .not_valid_after(now + datetime.timedelta(hours=1)).sign(tlskey, hashes.SHA256()))
        certfile = save("tls.crt", cert.public_bytes(serialization.Encoding.PEM))
        tlsfile = save("tls.key", tlskey.private_bytes(serialization.Encoding.PEM,
                        serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
        pin = hashlib.sha256(tlskey.public_key().public_bytes(serialization.Encoding.DER,
                    serialization.PublicFormat.SubjectPublicKeyInfo)).hexdigest()
        def port():
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                return sock.getsockname()[1]
        relay_port, control_port, tracker_port = port(), port(), port()
        def launch(binary, values):
            env = dict(os.environ, **values)
            process = subprocess.Popen([os.environ[binary]], env=env,
                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            def stop():
                process.terminate()
                try:
                    process.wait(timeout=4)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            self.addCleanup(stop)
            return process
        relay = launch("ARKNET_TEST_RELAY_BINARY", {
            "ARKNET_RELAY_ID": "probe", "ARKNET_RELAY_GRANT_VERIFY_KEY_FILE": grant_pub,
            "ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE": revoke_pub,
            "ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE": endpoint_key,
            "ARKNET_RELAY_TLS_CERT_FILE": certfile, "ARKNET_RELAY_TLS_KEY_FILE": tlsfile,
            "ARKNET_RELAY_BIND_ADDR": "127.0.0.1:" + str(relay_port),
            "ARKNET_RELAY_CONTROL_BIND_ADDR": "127.0.0.1:" + str(control_port)})
        tracker = launch("ARKNET_TEST_TRACKER_BINARY", {
            "ARKNET_TRACKER_VERIFY_KEY_FILE": grant_pub,
            "ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE": endpoint_pub,
            "ARKNET_TRACKER_BIND_ADDR": "127.0.0.1:" + str(tracker_port),
            "ARKNET_TRACKER_ANNOUNCE_INTERVAL": "1s"})
        def wait(predicate, seconds, message):
            deadline = time.monotonic() + seconds
            while time.monotonic() < deadline:
                if predicate():
                    return
                time.sleep(.05)
            self.fail(message)
        def ready(p):
            try:
                with socket.create_connection(("127.0.0.1", p), timeout=.1):
                    return True
            except OSError:
                return False
        wait(lambda: ready(tracker_port) and ready(relay_port), 5,
             "real binaries did not listen")
        self.assertIsNone(relay.poll())
        self.assertIsNone(tracker.poll())
        seed_dir, download_dir = root / "seed", root / "download"
        seed_dir.mkdir()
        download_dir.mkdir()
        size = 4 * 1024 * 1024
        (seed_dir / "payload").write_bytes(bytes(size))
        fs = lt.file_storage()
        lt.add_files(fs, str(seed_dir / "payload"))
        creator = lt.create_torrent(fs, 256 * 1024, lt.create_torrent.v2_only)
        creator.set_priv(True)
        lt.set_piece_hashes(creator, str(seed_dir))
        ti = lt.torrent_info(creator.generate())
        info_hash = str(ti.info_hashes().v2)
        expiry = int(time.time()) + 180
        origin = "http://127.0.0.1:" + str(tracker_port)
        def grant(action):
            payload = bytes([2]) + struct.pack("!Q", expiry) + secrets.token_bytes(16) + bytes.fromhex(info_hash) + bytes([action])
            encode = lambda data: base64.urlsafe_b64encode(data).rstrip(b"=").decode()
            # Tracker/internal/grant/grant.go verifies the raw payload, no domain.
            return origin + "/announce/" + encode(payload) + "." + encode(grant_key.sign(payload))
        configs = [{"id": "probe", "address": "127.0.0.1:" + str(relay_port), "spki_sha256": pin}]
        connectors, sessions, handles = [], [], []
        for index in range(2):
            connector = Connector(origin)
            connectors.append(connector)
            self.addCleanup(connector.close)
            # The product split: seeds listen unproxied on loopback; downloads
            # dial only through the SOCKS5 connector (worker.Manager).
            if index == 0:
                settings = seed_session_settings("127.0.0.1:0")
            else:
                settings = session_settings("127.0.0.1:0", 512 * 1024,
                                            allow_private_web_seed=True, proxy_port=connector.proxy_port)
            settings["alert_mask"] = lt.alert.category_t.all_categories
            session = lt.session(settings)
            sessions.append(session)
            if index == 0:
                connector.listen_port = session.listen_port()
                self.assertGreater(connector.listen_port, 0, "seed session opened no listener")
            url = grant(2 if index == 0 else 1)
            connector.authorize(info_hash, url, configs, expiry, seed=index == 0)
            if index == 0:
                wait(lambda: bool(connector._entries[info_hash].get("endpoint_token")),
                     5, "relay seed registration failed")
            params = lt.add_torrent_params()
            params.ti = ti
            params.save_path = str(seed_dir if index == 0 else download_dir)
            params.trackers = [connector.tracker_url(info_hash, url)]
            params.flags &= ~(lt.torrent_flags.paused | lt.torrent_flags.auto_managed)
            params.flags |= lt.torrent_flags.disable_dht | lt.torrent_flags.disable_lsd | lt.torrent_flags.disable_pex
            handles.append(session.add_torrent(params))
            if index == 0:
                wait(lambda: handles[0].status().is_seeding, 5, "seed failed hash check")
        max_peers = 0
        deadline = time.monotonic() + float(os.environ.get("ARKNET_TEST_RELAY_TIMEOUT", "35"))
        while time.monotonic() < deadline:
            statuses = [h.status() for h in handles]
            max_peers = max(max_peers, statuses[1].num_peers)
            diagnostics = {"seed": bool(statuses[0].is_seeding),
                "peers": statuses[1].num_peers, "done": statuses[1].total_wanted_done,
                "control": bool(connectors[0]._entries[info_hash].get("control")),
                "token": bool(connectors[0]._entries[info_hash].get("endpoint_token")),
                "tracker_replies": [sum(type(a).__name__ == "tracker_reply_alert" for a in s.pop_alerts()) for s in sessions]}
            print(json.dumps(diagnostics), flush=True)
            if statuses[1].is_seeding:
                break
            time.sleep(1)
        self.assertGreater(max_peers, 0, "tracker/relay path produced no libtorrent peers")
        self.assertEqual(handles[1].status().total_wanted_done, size,
                         "real relay did not deliver full payload")
        self.assertEqual((download_dir / "payload").read_bytes(), bytes(size))
        self.assertGreater(handles[0].status().total_payload_upload, 0)
        for session, handle in zip(sessions, handles):
            session.remove_torrent(handle)


if __name__ == "__main__":
    unittest.main()
