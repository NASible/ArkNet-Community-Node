"""Network-level tests for the product relay connector (no libtorrent needed)."""
import base64
import os
import time
import socket

HASH = "ab" * 32


def token(info_hash=HASH, action=1, expiry=None):
    payload = b"\x02" + int(expiry or time.time() + 120).to_bytes(8, "big") + b"s" * 16 + bytes.fromhex(info_hash) + bytes([action])
    return ".".join(base64.urlsafe_b64encode(v).rstrip(b"=").decode() for v in (payload, b"x" * 64))


def tracker(info_hash=HASH, action=1, expiry=None):
    return "http://127.0.0.1:9999/announce/" + token(info_hash, action, expiry)
import struct
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit, parse_qs
import contextlib
import io
import logging
from unittest.mock import patch
import unittest
import datetime
import hashlib
import json
import ssl
import tempfile
import threading
from pathlib import Path
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID


class FakeRelay:
    def __init__(self):
        self.temp = tempfile.TemporaryDirectory(dir=os.environ.get("ARKNET_SCRATCH"))
        key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "localhost")])
        now = datetime.datetime.now(datetime.timezone.utc)
        cert = (x509.CertificateBuilder().subject_name(name).issuer_name(name)
                .public_key(key.public_key()).serial_number(1)
                .not_valid_before(now - datetime.timedelta(days=1))
                .not_valid_after(now + datetime.timedelta(days=1)).sign(key, hashes.SHA256()))
        root = Path(self.temp.name)
        (root / "cert").write_bytes(cert.public_bytes(serialization.Encoding.PEM))
        (root / "key").write_bytes(key.private_bytes(serialization.Encoding.PEM,
            serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
        self.pin = hashlib.sha256(key.public_key().public_bytes(serialization.Encoding.DER,
            serialization.PublicFormat.SubjectPublicKeyInfo)).hexdigest()
        self.context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        self.context.minimum_version = ssl.TLSVersion.TLSv1_3
        self.context.load_cert_chain(str(root / "cert"), str(root / "key"))
        self.listener = socket.socket()
        self.listener.bind(("127.0.0.1", 0))
        self.listener.listen()
        self.listener.settimeout(.1)
        self.registration_expiry = 9999999999
        self.control = None
        self.reject_listen = False
        self.hold_accept = False
        self.accepted = []
        self.requests = []
        self.streams = []
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    @property
    def config(self):
        return dict(id="fake", address="127.0.0.1:" + str(self.listener.getsockname()[1]), spki_sha256=self.pin)

    def serve(self):
        while not self.stop.is_set():
            try:
                conn, _ = self.listener.accept()
            except OSError:
                continue
            threading.Thread(target=self.handle, args=(conn,), daemon=True).start()

    def handle(self, conn):
        try:
            with self.context.wrap_socket(conn, server_side=True) as s:
                s.settimeout(3)
                f = s.makefile("rb")
                req = json.loads(f.readline(4097))
                self.requests.append(req)
                if req["op"] == "connect":
                    s.sendall(b'{"ok":true}\n')
                    data = f.read(68)
                    self.streams.append(data)
                    s.sendall(data)
                    while True:
                        data = s.recv(4096)
                        if not data:
                            break
                        s.sendall(data)
                elif req["op"] == "renew":
                    s.sendall(b'{"op":"renewed","expires_at":9999999999}\n')
                elif req["op"] == "accept":
                    s.sendall(b'{"ok":true}\n' + handshake())
                    self.accepted.append(f.read(68))
                    while self.hold_accept:
                        data = s.recv(4096)
                        if not data:
                            break
                        s.sendall(data)
                elif req["op"] == "listen":
                    if self.reject_listen:
                        return
                    self.control = s
                    s.sendall(json.dumps(dict(endpoint="100.64.1.2:6881", endpoint_token="test-attestation", expires_at=self.registration_expiry)).encode() + b"\n")
                    while True:
                        req = json.loads(f.readline(4097))
                        self.requests.append(req)
                        s.sendall(b'{"op":"renewed","endpoint_token":"renewed-attestation","expires_at":9999999999}\n')
        except (OSError, ValueError):
            conn.close()

    def close(self):
        self.stop.set()
        self.listener.close()
        self.thread.join(1)
        self.temp.cleanup()


def handshake(info_hash=HASH):
    return b"\x13BitTorrent protocol" + b"\x00" * 8 + bytes.fromhex(info_hash)[:20] + b"p" * 20

try:
    from relay_connector import Connector
except ImportError:
    Connector = None


def receive(sock, n):
    data = b""
    while len(data) < n:
        chunk = sock.recv(n - len(data))
        if not chunk:
            break
        data += chunk
    return data


class ConnectorTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(Connector, "product Connector is missing")
        self.connector = Connector("http://127.0.0.1:9999")
        self.addCleanup(self.connector.close)

    def socks(self, atyp, address, command=1):
        s = socket.create_connection(("127.0.0.1", self.connector.proxy_port), timeout=2)
        self.addCleanup(s.close)
        s.sendall(b"\x05\x01\x00")
        self.assertEqual(receive(s, 2), b"\x05\x00")
        s.sendall(bytes([5, command, 0, atyp]) + address + struct.pack("!H", 6881))
        return s, receive(s, 10)

    def test_splice_preserves_backpressured_megabyte(self):
        source, left = socket.socketpair()
        right, sink = socket.socketpair()
        for sock in (source, left, right, sink):
            self.addCleanup(sock.close)
            sock.settimeout(8)
        right.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 4096)
        payload = bytes(range(256)) * 4096
        errors = []
        def send():
            try:
                source.sendall(payload)
            except OSError as exc:
                errors.append(exc)
        pump = threading.Thread(target=self.connector._splice,
            args=(None, None, left, right), daemon=True)
        pump.start()
        sender = threading.Thread(target=send, daemon=True)
        sender.start()
        time.sleep(.6)
        self.assertEqual(receive(sink, len(payload)), payload)
        sender.join(2)
        self.assertFalse(sender.is_alive())
        self.assertEqual(errors, [])
        source.close()
        sink.close()
        pump.join(2)
        self.assertFalse(pump.is_alive())

    def test_splice_tls_duplex_has_one_io_owner(self):
        relay = FakeRelay()
        self.addCleanup(relay.close)
        tls = self.connector._tls(relay.config)
        self.connector._send(tls, dict(op="connect", endpoint="100.64.1.2:6881", grant=token()))
        self.assertTrue(self.connector._json(tls)["ok"])
        tls.sendall(handshake())
        self.assertEqual(receive(tls, 68), handshake())
        owners = set()
        class OwnedTLS:
            def __getattr__(self, name):
                return getattr(tls, name)
            def recv(self, size):
                owners.add(threading.get_ident())
                return tls.recv(size)
            def send(self, data):
                owners.add(threading.get_ident())
                return tls.send(data)
            def sendall(self, data):
                owners.add(threading.get_ident())
                return tls.sendall(data)
        source, local = socket.socketpair()
        self.addCleanup(source.close)
        pump = threading.Thread(target=self.connector._splice,
            args=(None, None, OwnedTLS(), local), daemon=True)
        pump.start()
        source.settimeout(5)
        payload = bytes(range(256)) * 8192
        send_errors = []
        def upload():
            try:
                source.sendall(payload)
            except OSError as exc:
                send_errors.append(exc)
        sender = threading.Thread(target=upload, daemon=True)
        sender.start()
        self.assertEqual(receive(source, len(payload)), payload)
        sender.join(2)
        self.assertFalse(sender.is_alive())
        self.assertEqual(send_errors, [])
        source.close()
        pump.join(2)
        self.assertFalse(pump.is_alive())
        self.assertEqual(len(owners), 1, "SSL reads and writes must have one owner")

    def test_pump_retries_tls_readiness_and_partial_writes_without_loss(self):
        from relay_connector import _pump
        class ScriptedTLS:
            def __init__(self):
                self.reads = [ssl.SSLWantWriteError(), ssl.SSLWantReadError(), b"reply", b""]
                self.writes = [ssl.SSLWantReadError(), ssl.SSLWantWriteError(),
                               BlockingIOError(), 3, 2, 99]
                self.sent = bytearray()
                self.attempts = []
            def setblocking(self, blocking):
                self.blocking = blocking
            def recv(self, size):
                result = self.reads.pop(0)
                if isinstance(result, Exception):
                    raise result
                return result
            def send(self, data):
                self.attempts.append(bytes(data))
                result = self.writes.pop(0)
                if isinstance(result, Exception):
                    raise result
                count = min(result, len(data))
                self.sent.extend(data[:count])
                return count
        sock = ScriptedTLS()
        received = []
        outgoing = {sock: bytearray(b"abcdefghij")}
        iterations = []
        def alive():
            iterations.append(1)
            self.assertLess(len(iterations), 20, "readiness retry stalled")
            return True
        with patch("relay_connector.select.select", side_effect=lambda r, w, x, t: (r, w, [])):
            _pump((sock,), outgoing, lambda s: True,
                  lambda s, data: received.append(data), alive)
        self.assertFalse(sock.blocking)
        self.assertEqual(bytes(sock.sent), b"abcdefghij")
        self.assertEqual(received, [b"reply"])
        self.assertEqual(sock.attempts[:4], [b"abcdefghij"] * 4)
        self.assertEqual(sock.attempts[4:], [b"defghij", b"fghij"])

    def test_backpressured_splice_obeys_remove_expiry_and_close(self):
        for cancellation in ("remove", "grant", "list", "close"):
            with self.subTest(cancellation=cancellation):
                c = Connector("http://127.0.0.1:9999")
                self.addCleanup(c.close)
                config = dict(id="r", address="127.0.0.1:1", spki_sha256="00" * 32)
                c.authorize(HASH, tracker(), [config], int(time.time() + 100), False)
                entry = c._entries[HASH]
                source, left = socket.socketpair()
                right, sink = socket.socketpair()
                for sock in (source, left, right, sink):
                    self.addCleanup(sock.close)
                right.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 4096)
                source.settimeout(2)
                pump = threading.Thread(target=c._splice, args=(HASH, entry, left, right), daemon=True)
                pump.start()
                source.sendall(b"x" * 131072)
                time.sleep(.15)
                if cancellation == "remove":
                    c.remove(HASH)
                elif cancellation == "close":
                    c.close()
                else:
                    with c._lock:
                        entry["expiry" if cancellation == "grant" else "list_expiry"] = time.time() - 1
                pump.join(1)
                self.assertFalse(pump.is_alive(), "blocked output ignored cancellation")
                self.assertNotIn(left, c._sockets)
                self.assertNotIn(right, c._sockets)

    def test_splice_stops_reading_at_bounded_queue_limit(self):
        left, right = socket.socketpair()
        def inspect(sockets, outgoing, readable, received, alive):
            self.assertTrue(readable(left))
            received(left, b"x" * (65536 - 16384))
            self.assertTrue(readable(left))
            received(left, b"y" * 16384)
            self.assertFalse(readable(left), "full output queue must apply backpressure")
            self.assertTrue(readable(right), "reverse direction must remain live")
            outgoing[right].clear()
            self.assertTrue(readable(left))
        with patch("relay_connector._pump", side_effect=inspect):
            self.connector._splice(None, None, left, right)

    def test_relay_security_changes_close_existing_streams(self):
        config = dict(id="r", address="127.0.0.1:1", spki_sha256="00" * 32)
        for mutation in (dict(config, id="other"), dict(config, address="127.0.0.1:2"),
                         dict(config, spki_sha256="11" * 32), None):
            with self.subTest(relay=mutation):
                self.connector.authorize(HASH, tracker(), [config], int(time.time() + 100), False)
                peer, stream = socket.socketpair()
                self.addCleanup(peer.close)
                self.addCleanup(stream.close)
                peer.settimeout(1)
                entry = self.connector._entries[HASH]
                self.connector._track(HASH, entry, stream)
                self.connector.authorize(HASH, tracker(), [mutation] if mutation else [],
                    int(time.time() + 100), False)
                try:
                    self.assertEqual(peer.recv(1), b"")
                except socket.timeout:
                    self.fail("security-relevant relay change left the stream open")

    def test_authorize_validates_scope_action_expiry_and_origin(self):
        relay = [{"id": "r", "address": "127.0.0.1:1", "spki_sha256": "00" * 32}]
        for url, seed, deadline in [(tracker("cd" * 32), False, time.time() + 100),
                                    (tracker(action=2), False, time.time() + 100),
                                    (tracker(action=1), True, time.time() + 100),
                                    (tracker(action=0), False, time.time() + 100),
                                    (tracker(action=4), False, time.time() + 100),
                                    (tracker(expiry=time.time() - 1), False, time.time() + 100),
                                    (tracker(), False, time.time() - 1),
                                    (tracker().replace("9999", "9998"), False, time.time() + 100)]:
            with self.subTest(url=url, seed=seed):
                with self.assertRaises(ValueError):
                    self.connector.authorize(HASH, url, relay, int(deadline), seed)
        self.connector.authorize(HASH, tracker(), relay, int(time.time() + 100), False)

    def test_empty_relay_list_allows_legacy_webseed_configuration(self):
        url = "http://127.0.0.1:9999/announce/YWJj.ZGVm"
        self.connector.authorize(HASH, url, [], 0, False)
        local = self.connector.tracker_url(HASH, url)
        self.assertTrue(local.startswith("http://127.0.0.1:"))
        self.assertNotIn("YWJj", local)
        s, reply = self.socks(1, socket.inet_aton("100.64.1.1"))
        self.assertEqual(reply[1], 0)
        s.sendall(b"\x13BitTorrent protocol" + b"\x00" * 8 + bytes.fromhex(HASH)[:20] + b"p" * 20)
        self.assertEqual(s.recv(1), b"")

    def test_pinned_relay_replays_handshake_and_remove_closes_stream(self):
        relay = FakeRelay()
        self.addCleanup(relay.close)
        self.connector.authorize(HASH, tracker(), [relay.config], int(time.time() + 100), False)
        s, _ = self.socks(1, socket.inet_aton("100.64.1.2"))
        s.sendall(handshake())
        self.assertEqual(receive(s, 68), handshake())
        s.sendall(b"payload")
        self.assertEqual(receive(s, 7), b"payload")
        self.assertEqual(relay.requests[0]["endpoint"], "100.64.1.2:6881")
        self.connector.remove(HASH)
        self.assertEqual(s.recv(1), b"")

    def test_wrong_pin_unknown_hash_and_ambiguous_prefix_send_no_grant(self):
        relay = FakeRelay()
        self.addCleanup(relay.close)
        bad = dict(relay.config, spki_sha256="00" * 32)
        self.connector.authorize(HASH, tracker(), [bad], int(time.time() + 100), False)
        s, _ = self.socks(1, socket.inet_aton("100.64.1.2"))
        s.sendall(handshake())
        self.assertEqual(s.recv(1), b"")
        self.assertEqual(relay.requests, [])
        self.connector.authorize(HASH, tracker(), [relay.config], int(time.time() + 100), False)
        other = HASH[:40] + "cd" * 12
        self.connector.authorize(other, tracker(other), [relay.config], int(time.time() + 100), False)
        for value in [HASH, "ef" * 32]:
            s, _ = self.socks(1, socket.inet_aton("100.64.1.2"))
            s.sendall(handshake(value))
            self.assertEqual(s.recv(1), b"")
        self.assertEqual(relay.requests, [])

    def origin(self, body=b"d8:intervali30e5:peers0:e"):
        paths = []
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                paths.append(self.path)
                self.send_response(200)
                self.end_headers()
                self.wfile.write(body)
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        origin = "http://127.0.0.1:" + str(server.server_port)
        connector = Connector(origin)
        self.addCleanup(connector.close)
        return connector, origin, paths

    def announce(self, url, query="left=1"):
        p = urlsplit(url)
        conn = http.client.HTTPConnection(p.hostname, p.port, timeout=3)
        try:
            conn.request("GET", p.path + "?" + query)
            response = conn.getresponse()
            self.assertEqual(response.status, 200)
            return response.read()
        finally:
            conn.close()

    def test_tracker_bridge_exact_origin_download_forward_seed_fail_and_no_logs(self):
        connector, origin, paths = self.origin()
        url = origin + "/announce/YWJj.ZGVm"
        connector.authorize(HASH, url, [], 0, False)
        local = connector.tracker_url(HASH, url)
        out, err = io.StringIO(), io.StringIO()
        logs = io.StringIO()
        handler = logging.StreamHandler(logs)
        logging.getLogger().addHandler(handler)
        try:
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                self.assertIn(b"interval", self.announce(local))
                self.assertEqual(paths, ["/announce/YWJj.ZGVm?left=1"])
                self.assertIn(b"failure reason", self.announce(local, "left=0"))
                self.assertIn(b"failure reason", self.announce(local, "left=1&endpoint_token=evil"))
                self.assertIn(b"failure reason", self.announce(local, "left=1&left=0"))
        finally:
            logging.getLogger().removeHandler(handler)
        self.assertEqual(len(paths), 1)
        self.assertEqual(out.getvalue() + err.getvalue() + logs.getvalue(), "")

    def test_seed_announce_reply_carries_no_peer_endpoints(self):
        # Seeds run in an unproxied session; they must never learn an endpoint
        # they could dial. Downloaders still receive the virtual endpoints.
        peers = socket.inet_aton("100.64.1.2") + struct.pack("!H", 6881)
        peers6 = socket.inet_pton(socket.AF_INET6, "fd00::1") + struct.pack("!H", 6881)
        body = (b"d8:completei1e8:intervali30e5:peers6:" + peers + b"6:peers618:" + peers6 + b"e")
        connector, origin, paths = self.origin(body)
        relay = FakeRelay()
        self.addCleanup(relay.close)
        url = origin + "/announce/" + token(action=2)
        connector.authorize(HASH, url, [relay.config], int(time.time() + 100), True)
        local = connector.tracker_url(HASH, url)
        self.wait_for(lambda: relay.control is not None)
        reply = b""
        def answered():
            nonlocal reply
            reply = self.announce(local, "left=0")
            return b"interval" in reply
        self.wait_for(answered)
        self.assertEqual(reply, b"d8:completei1e8:intervali30e5:peers0:e")
        self.assertNotIn(socket.inet_aton("100.64.1.2"), reply)
        download_hash = "cd" * 32
        download = origin + "/announce/" + token(download_hash, action=1)
        connector.authorize(download_hash, download, [relay.config], int(time.time() + 100), False)
        self.assertEqual(self.announce(connector.tracker_url(download_hash, download), "left=1"), body)

    def test_malformed_seed_reply_is_a_sanitized_failure(self):
        for body in (b"not bencode", b"d5:peersi1ee", b"d8:intervali30e5:peers0:", b"d8:intervali30e5:peers0:ex"):
            with self.subTest(body=body):
                connector, origin, _ = self.origin(body)
                relay = FakeRelay()
                self.addCleanup(relay.close)
                url = origin + "/announce/" + token(action=2)
                connector.authorize(HASH, url, [relay.config], int(time.time() + 100), True)
                local = connector.tracker_url(HASH, url)
                self.wait_for(lambda: relay.control is not None)
                self.wait_for(lambda: connector._entries[HASH].get("endpoint_token"))
                self.assertIn(b"failure reason", self.announce(local, "left=0"))

    def wait_for(self, predicate):
        deadline = time.monotonic() + 3
        while not predicate():
            if time.monotonic() >= deadline:
                self.fail("condition not reached")
            time.sleep(.02)

    def test_seed_registration_injection_renewal_and_accept_to_loopback(self):
        connector, origin, paths = self.origin()
        relay = FakeRelay()
        self.addCleanup(relay.close)
        url = origin + "/announce/" + token(action=2)
        connector.authorize(HASH, url, [relay.config], int(time.time() + 100), True)
        local = connector.tracker_url(HASH, url)
        self.wait_for(lambda: relay.control is not None)
        self.wait_for(lambda: b"interval" in self.announce(local, "left=0"))
        self.assertEqual(parse_qs(urlsplit(paths[-1]).query)["endpoint_token"], ["test-attestation"])
        updated = origin + "/announce/" + token(action=2, expiry=time.time() + 150)
        connector.authorize(HASH, updated, [relay.config], int(time.time() + 100), True)
        local = connector.tracker_url(HASH, updated)
        self.wait_for(lambda: any(r["op"] == "renew" for r in relay.requests))
        self.wait_for(lambda: b"interval" in self.announce(local, "left=0") and
                      "renewed-attestation" in paths[-1])
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        listener.settimeout(2)
        self.addCleanup(listener.close)
        connector.listen_port = listener.getsockname()[1]
        relay.control.sendall(b'{"op":"incoming","session":"0123456789abcdef0123456789abcdef"}\n')
        peer, address = listener.accept()
        with peer:
            self.assertEqual(address[0], "127.0.0.1")
            data = receive(peer, 68)
            self.assertEqual(data, handshake())
            peer.sendall(data)
        self.wait_for(lambda: relay.accepted == [handshake()])
        connector.remove(HASH)
        self.assertIn(b"failure reason", self.announce(local, "left=0"))

    def test_seed_refresh_during_registration_is_not_lost(self):
        relay = FakeRelay()
        self.addCleanup(relay.close)
        registration = threading.Event()
        release = threading.Event()
        self.addCleanup(release.set)
        original = self.connector._json
        def delayed(conn):
            reply = original(conn)
            if "endpoint" in reply:
                registration.set()
                release.wait(2)
            return reply
        with patch.object(self.connector, "_json", side_effect=delayed):
            self.connector.authorize(HASH, tracker(action=2), [relay.config], int(time.time() + 100), True)
            self.assertTrue(registration.wait(2))
            updated = tracker(action=2, expiry=time.time() + 150)
            # Run renewal synchronously to expose the pre-publication window.
            self.connector.authorize(HASH, updated, [relay.config], int(time.time() + 100), True)
            self.connector._renew(HASH, self.connector._entries[HASH])
            release.set()
            self.wait_for(lambda: any(r["op"] == "renew" and
                r["grant"] == updated.rsplit("/", 1)[1] for r in relay.requests))

    def test_seed_control_renewals_have_one_tls_owner(self):
        relay = FakeRelay()
        self.addCleanup(relay.close)
        owners = {}
        original = self.connector._tls
        class OwnedTLS:
            def __init__(self, sock):
                self.sock = sock
                owners[self] = set()
            def __getattr__(self, name):
                return getattr(self.sock, name)
            def recv(self, n):
                owners[self].add(threading.get_ident())
                return self.sock.recv(n)
            def send(self, data):
                owners[self].add(threading.get_ident())
                return self.sock.send(data)
            def sendall(self, data):
                owners[self].add(threading.get_ident())
                return self.sock.sendall(data)
        with patch.object(self.connector, "_tls", side_effect=lambda r: OwnedTLS(original(r))):
            self.connector.authorize(HASH, tracker(action=2), [relay.config], int(time.time() + 100), True)
            self.wait_for(lambda: self.connector._entries[HASH].get("control"))
            control = self.connector._entries[HASH]["control"]
            for i in range(8):
                self.connector.authorize(HASH, tracker(action=2, expiry=time.time() + 150 + i),
                    [relay.config], int(time.time() + 100), True)
                self.wait_for(lambda: len([r for r in relay.requests if r["op"] == "renew"]) > i)
            self.assertEqual(len(owners[control]), 1, "control read and renewal write must share owner")

    def test_seed_old_relay_stream_is_renewed_after_control_failover(self):
        first, second = FakeRelay(), FakeRelay()
        for relay in (first, second):
            self.addCleanup(relay.close)
        first.hold_accept = True
        configs = [dict(first.config, id="first"), dict(second.config, id="second")]
        listener = socket.socket()
        self.addCleanup(listener.close)
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        listener.settimeout(3)
        self.connector.listen_port = listener.getsockname()[1]
        self.connector.authorize(HASH, tracker(action=2), configs, int(time.time() + 100), True)
        self.wait_for(lambda: first.control is not None)
        first.control.sendall(b'{"op":"incoming","session":"0123456789abcdef0123456789abcdef"}\n')
        peer, _ = listener.accept()
        self.addCleanup(peer.close)
        peer.settimeout(3)
        self.assertEqual(receive(peer, 68), handshake())
        peer.sendall(handshake())
        self.wait_for(lambda: first.accepted == [handshake()])
        first.reject_listen = True
        first.control.shutdown(socket.SHUT_RDWR)
        self.wait_for(lambda: second.control is not None)
        self.wait_for(lambda: self.connector._entries[HASH].get("control") is not None and
            self.connector._entries[HASH]["control"].getpeername()[1] == second.listener.getsockname()[1])
        self.connector.authorize(HASH, tracker(action=2, expiry=time.time() + 150),
            configs, int(time.time() + 100), True)
        self.wait_for(lambda: any(r["op"] == "renew" for r in second.requests))
        self.wait_for(lambda: any(r["op"] == "renew" for r in first.requests))
        peer.sendall(b"old stream")
        self.assertEqual(receive(peer, 10), b"old stream")

    def test_reprioritization_preserves_stream_and_changes_next_dial(self):
        first, second = FakeRelay(), FakeRelay()
        for relay in (first, second):
            self.addCleanup(relay.close)
        configs = [dict(first.config, id="first"), dict(second.config, id="second")]
        self.connector.authorize(HASH, tracker(), configs, int(time.time() + 100), False)
        peer, _ = self.socks(1, socket.inet_aton("100.64.1.2"))
        peer.sendall(handshake())
        self.assertEqual(receive(peer, 68), handshake())
        self.connector.authorize(HASH, tracker(expiry=time.time() + 150), configs[::-1], int(time.time() + 100), False)
        peer.sendall(b"preserved")
        self.assertEqual(receive(peer, 9), b"preserved")
        next_peer, _ = self.socks(1, socket.inet_aton("100.64.1.2"))
        next_peer.sendall(handshake())
        self.assertEqual(receive(next_peer, 68), handshake())
        self.assertEqual(len(first.streams), 1)
        self.assertEqual(len(second.streams), 1)

    def test_download_refresh_renews_without_interrupting_stream(self):
        relay = FakeRelay()
        self.addCleanup(relay.close)
        self.connector.authorize(HASH, tracker(), [relay.config], int(time.time() + 100), False)
        s, _ = self.socks(1, socket.inet_aton("100.64.1.2"))
        s.sendall(handshake())
        self.assertEqual(receive(s, 68), handshake())
        self.connector.authorize(HASH, tracker(expiry=time.time() + 150), [relay.config], int(time.time() + 100), False)
        self.wait_for(lambda: any(r["op"] == "renew" for r in relay.requests))
        s.sendall(b"still live")
        self.assertEqual(receive(s, 10), b"still live")

    def test_exact_webseed_origin_works_without_relays_and_other_port_is_refused(self):
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        listener.settimeout(2)
        self.addCleanup(listener.close)
        port = listener.getsockname()[1]
        c = Connector("http://127.0.0.1:9999", web_seed_origin="http://localhost:" + str(port))
        self.addCleanup(c.close)
        c.authorize(HASH, tracker(), [{"id": "down", "address": "127.0.0.1:1", "spki_sha256": "00" * 32}], int(time.time() + 100), False)
        def request(host, dest_port, domain=False):
            s = socket.create_connection(("127.0.0.1", c.proxy_port), timeout=2)
            self.addCleanup(s.close)
            s.sendall(b"\x05\x01\x00")
            self.assertEqual(receive(s, 2), b"\x05\x00")
            addr = bytes([len(host)]) + host.encode() if domain else socket.inet_aton(host)
            s.sendall(b"\x05\x01\x00" + (b"\x03" if domain else b"\x01") + addr + struct.pack("!H", dest_port))
            return s, receive(s, 10)
        for host, domain in [("localhost", True), ("127.0.0.1", False)]:
            s, reply = request(host, port, domain)
            self.assertEqual(reply[1], 0)
            s.sendall(b"GET /payload HTTP/1.0\r\n\r\n")
            peer, _ = listener.accept()
            with peer:
                self.assertEqual(receive(peer, 25), b"GET /payload HTTP/1.0\r\n\r\n")
                peer.sendall(b"webseed bytes")
                self.assertEqual(receive(s, 13), b"webseed bytes")
        for host, dest_port, domain in [("127.0.0.1", port + 1, False), ("example.org", port, True)]:
            _, reply = request(host, dest_port, domain)
            self.assertEqual(reply[1], 2)

    def test_endpoint_token_expiry_refuses_complete_announce(self):
        connector, origin, paths = self.origin()
        relay = FakeRelay()
        self.addCleanup(relay.close)
        relay.registration_expiry = int(time.time()) + 2
        url = origin + "/announce/" + token(action=2)
        connector.authorize(HASH, url, [relay.config], int(time.time() + 100), True)
        local = connector.tracker_url(HASH, url)
        self.wait_for(lambda: b"interval" in self.announce(local, "left=0"))
        time.sleep(max(0, relay.registration_expiry - time.time()) + .15)
        count = len(paths)
        self.assertIn(b"failure reason", self.announce(local, "left=0"))
        self.assertEqual(len(paths), count)

    def test_tracker_route_remains_stable_and_uses_refreshed_exact_url(self):
        connector, origin, paths = self.origin()
        first = origin + "/announce/YWJj.ZGVm"
        second = origin + "/announce/YWJj.ZGVn"
        connector.authorize(HASH, first, [], 0, False)
        local = connector.tracker_url(HASH, first)
        connector.authorize(HASH, second, [], 0, False)
        self.assertEqual(connector.tracker_url(HASH, second), local)
        self.assertIn(b"interval", self.announce(local))
        self.assertTrue(paths[-1].startswith("/announce/YWJj.ZGVn?"))

    def test_active_streams_close_at_grant_or_list_expiry_and_lists_are_dropped(self):
        for source in ("grant", "list"):
            with self.subTest(source=source):
                relay = FakeRelay()
                self.addCleanup(relay.close)
                deadline = int(time.time()) + 2
                self.connector.authorize(HASH, tracker(expiry=deadline if source == "grant" else time.time() + 100),
                    [relay.config], deadline if source == "list" else int(time.time() + 100), False)
                s, _ = self.socks(1, socket.inet_aton("100.64.1.2"))
                s.sendall(handshake())
                self.assertEqual(receive(s, 68), handshake())
                s.settimeout(3)
                self.assertEqual(s.recv(1), b"")
                self.wait_for(lambda: not self.connector._entries[HASH]["relays"])
                self.connector.remove(HASH)

    def test_seed_control_outage_reconnects_without_leaking_errors(self):
        relay = FakeRelay()
        self.addCleanup(relay.close)
        self.connector.authorize(HASH, tracker(action=2), [relay.config], int(time.time() + 100), True)
        self.wait_for(lambda: relay.control is not None)
        relay.control.shutdown(socket.SHUT_RDWR)
        self.wait_for(lambda: len([r for r in relay.requests if r["op"] == "listen"]) >= 2)

    def test_registration_reply_rejects_malformed_endpoints(self):
        for endpoint in [123, [], "127.0.0.1:6881", "100.64.0.1:0", "[::1]:6881"]:
            with self.subTest(endpoint=endpoint):
                with self.assertRaises(ValueError):
                    self.connector._endpoint(dict(expiry=time.time() + 10, list_expiry=time.time() + 10),
                        dict(endpoint=endpoint, endpoint_token="opaque", expires_at=int(time.time() + 10)))

    def test_background_work_is_bounded_and_slots_are_released(self):
        with patch("relay_connector.MAX_CONNECTIONS", 2):
            c = Connector("http://127.0.0.1:9999")
            self.addCleanup(c.close)
            release = threading.Event()
            try:
                self.assertTrue(c._spawn(release.wait))
                self.assertTrue(c._spawn(release.wait))
                self.assertFalse(c._spawn(release.wait))
            finally:
                release.set()
            self.wait_for(lambda: c._spawn(lambda: None))

    def test_nested_nonobject_relay_json_is_a_sanitized_protocol_error(self):
        left, right = socket.socketpair()
        with left, right:
            left.sendall(b"[" * 1500 + b"0" + b"]" * 1500 + b"\n")
            with self.assertRaises(ValueError):
                self.connector._json(right)

    def test_refuses_real_ipv4_domains_ipv6_and_udp(self):
        _, reply = self.socks(1, socket.inet_aton("100.64.1.1"), command=3)
        self.assertEqual(reply[1], 2)
        for atyp, address in [(1, socket.inet_aton("127.0.0.1")),
                              (1, socket.inet_aton("100.128.0.1")),
                              (3, b"\x09localhost"), (4, b"\x00" * 15 + b"\x01")]:
            with self.subTest(atyp=atyp, address=address):
                _, reply = self.socks(atyp, address)
                self.assertEqual(reply[1], 2)


if __name__ == "__main__":
    unittest.main()
