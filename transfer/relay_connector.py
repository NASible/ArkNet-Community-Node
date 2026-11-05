"""Fail-closed loopback transport for ArkNet's pinned TLS relay protocol.

No credentials or peer addresses are logged. Core authenticates relay lists;
this module checks their deadlines, while relays verify grant signatures.
"""
import base64
import re
import secrets
import time
from urllib.parse import urlsplit, unquote_to_bytes, quote
import http.client
import ssl
import json
import hashlib
from cryptography import x509
from cryptography.hazmat.primitives import serialization
import ipaddress
import socket
import threading
import select


def _pump(sockets, outgoing, readable, received, alive, tick=lambda: None):
    """Own all TLS I/O; bounded buffers and stable partial-write retries.

    A TLS read may need writability and a TLS write may need readability.
    Readiness is a hint, never a reason to discard queued application bytes.
    """
    read_wait = {s: "r" for s in sockets}
    write_wait = {s: "w" for s in sockets}
    pending = {s: b"" for s in sockets}
    eof = False
    for sock in sockets:
        sock.setblocking(False)
    while alive():
        tick()
        if eof and not any(outgoing.values()):
            return
        reads, writes = set(), set()
        for sock in sockets:
            if not eof and readable(sock):
                (reads if read_wait[sock] == "r" else writes).add(sock)
            if outgoing[sock]:
                (reads if write_wait[sock] == "r" else writes).add(sock)
        # SSL may have decrypted bytes even when its fd is not readable.
        buffered = {s for s in sockets if not eof and readable(s)
                    and getattr(s, "pending", lambda: 0)()}
        ready_r, ready_w, _ = select.select(list(reads), list(writes), [], 0 if buffered else .1)
        ready_r, ready_w = set(ready_r) | buffered, set(ready_w)
        for sock in sockets:
            if outgoing[sock] and sock in (ready_r if write_wait[sock] == "r" else ready_w):
                if not pending[sock]:
                    pending[sock] = bytes(outgoing[sock][:16384])
                try:
                    count = sock.send(pending[sock])
                    if not count:
                        raise OSError("closed")
                    del outgoing[sock][:count]
                    pending[sock] = pending[sock][count:]
                    write_wait[sock] = "w"
                except ssl.SSLWantReadError:
                    write_wait[sock] = "r"
                except (ssl.SSLWantWriteError, BlockingIOError):
                    write_wait[sock] = "w"
            if not eof and readable(sock) and sock in (ready_r if read_wait[sock] == "r" else ready_w):
                try:
                    data = sock.recv(16384)
                    read_wait[sock] = "r"
                    if not data:
                        eof = True
                    else:
                        received(sock, data)
                except ssl.SSLWantWriteError:
                    read_wait[sock] = "w"
                except (ssl.SSLWantReadError, BlockingIOError):
                    read_wait[sock] = "r"

VIRTUAL = ipaddress.ip_network("100.64.0.0/10")
TIMEOUT = 3
MAX_CONNECTIONS = 128


def _bdecode(data, i=0, depth=0):
    """Strict bencode: canonical integers, sorted unique dictionary keys."""
    if depth > 16 or i >= len(data):
        raise ValueError("invalid bencode")
    c = data[i:i + 1]
    if c == b"i":
        end = data.index(b"e", i)
        text = data[i + 1:end]
        if not re.fullmatch(rb"0|-?[1-9][0-9]{0,18}", text):
            raise ValueError("invalid bencode")
        return int(text), end + 1
    if c == b"l" or c == b"d":
        i, items = i + 1, []
        while data[i:i + 1] != b"e":
            value, i = _bdecode(data, i, depth + 1)
            items.append(value)
            if len(items) > 4096:
                raise ValueError("invalid bencode")
        if c == b"l":
            return items, i + 1
        keys = items[0::2]
        if (len(items) % 2 or not all(isinstance(k, bytes) for k in keys) or
                keys != sorted(set(keys))):
            raise ValueError("invalid bencode")
        return dict(zip(keys, items[1::2])), i + 1
    colon = data.index(b":", i)
    text = data[i:colon]
    if not re.fullmatch(rb"0|[1-9][0-9]{0,7}", text) or colon + 1 + int(text) > len(data):
        raise ValueError("invalid bencode")
    return data[colon + 1:colon + 1 + int(text)], colon + 1 + int(text)


def _bencode(value):
    if isinstance(value, int):
        return b"i%de" % value
    if isinstance(value, bytes):
        return b"%d:%s" % (len(value), value)
    if isinstance(value, list):
        return b"l" + b"".join(map(_bencode, value)) + b"e"
    return b"d" + b"".join(_bencode(k) + _bencode(value[k]) for k in sorted(value)) + b"e"


def _seed_reply(data):
    """A seed never needs, and so never receives, a peer endpoint to dial."""
    reply, end = _bdecode(data)
    if end != len(data) or not isinstance(reply, dict):
        raise ValueError("invalid tracker reply")
    if b"failure reason" not in reply:
        if not isinstance(reply.get(b"interval"), int) or b"peers" not in reply:
            raise ValueError("invalid tracker reply")
        reply[b"peers"] = b""
        reply.pop(b"peers6", None)
    return _bencode(reply)


def _close(sock):
    try:
        sock.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass
    sock.close()


def _read(sock, count):
    data = b""
    while len(data) < count:
        chunk = sock.recv(count - len(data))
        if not chunk:
            raise OSError("closed")
        data += chunk
    return data


class Connector:
    def __init__(self, tracker_origin: str, listen_port: int = 6881, web_seed_origin: str = ""):
        self._webseed_host = ""
        self._webseed_port = 0
        self._webseed_addresses = set()
        if web_seed_origin:
            w = urlsplit(web_seed_origin)
            if (w.scheme not in ("http", "https") or not w.hostname or w.username or w.password
                    or w.path or w.query or w.fragment):
                raise ValueError("invalid webseed origin")
            self._webseed_host = w.hostname
            self._webseed_port = w.port or (443 if w.scheme == "https" else 80)
            try:
                self._webseed_addresses = {item[4][0] for item in socket.getaddrinfo(
                    self._webseed_host, self._webseed_port, type=socket.SOCK_STREAM)}
            except OSError:
                pass  # An origin DNS outage does not disable relay operation.

        self._origin = tracker_origin
        parsed = urlsplit(tracker_origin)
        if (parsed.scheme not in ("http", "https") or not parsed.hostname or
                parsed.username or parsed.password or parsed.path or parsed.query or parsed.fragment):
            raise ValueError("invalid tracker origin")
        if not 1 <= listen_port <= 65535:
            raise ValueError("invalid listen port")
        self._entries = {}
        self._routes = {}
        self.listen_port = listen_port
        self._lock = threading.RLock()
        self._stop = threading.Event()
        self._sockets = set()
        self._slots = threading.BoundedSemaphore(MAX_CONNECTIONS)
        self._listener = socket.socket()
        self._listener.bind(("127.0.0.1", 0))
        self._listener.listen(32)
        self._listener.settimeout(.2)
        self.proxy_port = self._listener.getsockname()[1]
        self._thread = threading.Thread(target=self._serve, daemon=True)
        self._thread.start()
        self._http_listener = socket.socket()
        self._http_listener.bind(("127.0.0.1", 0))
        self._http_listener.listen(32)
        self._http_listener.settimeout(.2)
        self._http_thread = threading.Thread(target=self._serve,
            args=(self._http_listener, self._http), daemon=True)
        self._http_thread.start()
        self._sweep_thread = threading.Thread(target=self._sweep, daemon=True)
        self._sweep_thread.start()

    def _url(self, info_hash, url):
        if not isinstance(info_hash, str) or not re.fullmatch(r"[0-9a-f]{64}", info_hash):
            raise ValueError("invalid hash")
        if not isinstance(url, str) or len(url) > 2048 or any(c in url for c in "%\\\r\n"):
            raise ValueError("invalid tracker")
        p = urlsplit(url)
        if (p.scheme + "://" + p.netloc != self._origin or p.query or p.fragment or
                p.username or p.password or not re.fullmatch(r"/announce/[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+", p.path)):
            raise ValueError("invalid tracker")
        return p.path[len("/announce/"):]

    def authorize(self, info_hash_hex: str, tracker_url: str, relays: list,
                  relay_list_expires_at: int, seed: bool) -> None:
        grant = self._url(info_hash_hex, tracker_url)
        expiry = 0
        subject = b""
        if relays:
            if len(relays) > 16 or time.time() >= relay_list_expires_at:
                raise ValueError("expired relay list")
            parts = grant.split(".")
            decoded = []
            for part in parts:
                value = base64.urlsafe_b64decode(part + "=" * (-len(part) % 4))
                if base64.urlsafe_b64encode(value).rstrip(b"=").decode() != part:
                    raise ValueError("invalid grant")
                decoded.append(value)
            payload, signature = decoded
            if (len(payload) != 58 or len(signature) != 64 or payload[0] != 2 or
                    payload[25:57].hex() != info_hash_hex or payload[57] not in (1, 2, 3) or
                    not payload[57] & (2 if seed else 1)):
                raise ValueError("invalid grant scope")
            expiry = int.from_bytes(payload[1:9], "big")
            subject = payload[9:25]
            if time.time() >= expiry:
                raise ValueError("expired grant")
            for relay in relays:
                if (not isinstance(relay, dict) or not isinstance(relay.get("id"), str) or
                        not isinstance(relay.get("address"), str) or
                        not re.fullmatch(r"[0-9a-fA-F]{64}", relay.get("spki_sha256", ""))):
                    raise ValueError("invalid relay")
                address = urlsplit("//" + relay["address"])
                if (not address.hostname or not address.port or address.username or address.password
                        or address.path or address.query or address.fragment):
                    raise ValueError("invalid relay")
        with self._lock:
            if self._stop.is_set():
                raise ValueError("connector closed")
            entry = self._entries.get(info_hash_hex)
            # Priority changes affect future dials, not existing authorizations.
            relay_set = lambda items: {json.dumps(r, sort_keys=True) for r in items}
            changed = (entry is None or entry["subject"] != subject or entry["seed"] != seed
                       or relay_set(entry["relays"]) != relay_set(relays))
            if changed:
                if entry:
                    for sock in list(entry.get("sockets", ())):
                        _close(sock)
                entry = dict(sockets=set(), used_relays={})
            entry.update(url=tracker_url, grant=grant, relays=[dict(r) for r in relays],
                list_expiry=relay_list_expires_at, expiry=expiry, seed=seed, subject=subject,
                strict=bool(relays))
            self._entries[info_hash_hex] = entry
            if changed and seed and relays:
                self._spawn(self._seed_loop, info_hash_hex, entry)
            elif not changed and relays:
                self._spawn(self._renew, info_hash_hex, entry)

    def tracker_url(self, info_hash_hex: str, original_tracker_url: str) -> str:
        self._url(info_hash_hex, original_tracker_url)
        with self._lock:
            key = (info_hash_hex, original_tracker_url)
            if key not in self._routes:
                previous = next((k for k in self._routes if k[0] == info_hash_hex), None)
                if previous is not None:
                    self._routes[key] = self._routes.pop(previous)
                else:
                    if len(self._routes) >= 2048:
                        raise ValueError("too many routes")
                    self._routes[key] = secrets.token_urlsafe(24)
            return "http://127.0.0.1:" + str(self._http_listener.getsockname()[1]) + "/announce/" + self._routes[key]

    def _sweep(self):
        while not self._stop.wait(.1):
            with self._lock:
                for entry in self._entries.values():
                    if entry["strict"] and not self._live(entry):
                        entry["relays"] = []
                        entry.pop("endpoint_token", None)
                        for sock in list(entry["sockets"]):
                            _close(sock)

    def _spawn(self, function, *args):
        if self._stop.is_set() or not self._slots.acquire(blocking=False):
            return False

        def run():
            try:
                function(*args)
            finally:
                self._slots.release()
        threading.Thread(target=run, daemon=True).start()
        return True

    def _renew(self, info_hash, entry):
        with self._lock:
            if self._entries.get(info_hash) is not entry or not self._live(entry):
                return
            grant = entry["grant"]
            relays = list(entry["used_relays"].values())
            if entry["seed"]:
                # Coalesce refreshes, including ones arriving during registration.
                # Only the control owner touches its SSLSocket.
                entry["pending_renewal"] = grant
        for relay in relays:
            conn = None
            try:
                conn = self._tls(relay)
                self._track(info_hash, entry, conn)
                self._send(conn, dict(op="renew", grant=grant))
                if self._json(conn).get("op") != "renewed":
                    raise OSError("renewal refused")
            except (OSError, ValueError):
                # A failed renewal cannot extend sessions at the relay.
                pass
            finally:
                if conn:
                    _close(conn)
                    with self._lock:
                        self._sockets.discard(conn)
                        entry["sockets"].discard(conn)

    def _endpoint(self, entry, message):
        token = message.get("endpoint_token")
        expiry = message.get("expires_at")
        if not isinstance(token, str) or not 1 <= len(token) <= 1024 or not isinstance(expiry, int) or expiry <= time.time():
            raise ValueError("invalid registration")
        if "endpoint" in message:
            if not isinstance(message["endpoint"], str):
                raise ValueError("invalid endpoint")
            host, port = message["endpoint"].rsplit(":", 1)
            if ipaddress.ip_address(host) not in VIRTUAL or not 1024 <= int(port) <= 65535:
                raise ValueError("invalid endpoint")
        entry["endpoint_token"] = token
        entry["endpoint_expiry"] = min(expiry, entry["expiry"], entry["list_expiry"])

    def _seed_loop(self, info_hash, entry):
        backoff = .25
        while not self._stop.is_set():
            with self._lock:
                if self._entries.get(info_hash) is not entry or not self._live(entry):
                    return
                relays = list(entry["relays"])
            for relay in relays:
                conn = None
                try:
                    conn = self._tls(relay)
                    self._track(info_hash, entry, conn)
                    self._send(conn, dict(op="listen", grant=entry["grant"]))
                    reply = self._json(conn)
                    if "endpoint" not in reply:
                        raise ValueError("missing endpoint")
                    with self._lock:
                        if self._stop.is_set() or (entry is not None and
                                (self._entries.get(info_hash) is not entry or not self._live(entry))):
                            return
                        self._endpoint(entry, reply)
                        entry["control"] = conn
                    backoff = .25
                    self._control(info_hash, entry, relay, conn)
                except (OSError, ValueError):
                    pass
                finally:
                    if conn:
                        _close(conn)
                        with self._lock:
                            self._sockets.discard(conn)
                            entry["sockets"].discard(conn)
                            if entry.get("control") is conn:
                                entry.pop("control", None)
                                entry.pop("endpoint_token", None)
            if self._stop.wait(backoff):
                return
            backoff = min(4, backoff * 2)

    def _control(self, info_hash, entry, relay, conn):
        outgoing = {conn: bytearray()}
        line = bytearray()
        def alive():
            with self._lock:
                return (not self._stop.is_set() and self._entries.get(info_hash) is entry
                        and self._live(entry))
        def tick():
            with self._lock:
                if not outgoing[conn]:
                    grant = entry.pop("pending_renewal", None)
                    if grant:
                        outgoing[conn].extend(json.dumps(dict(op="renew", grant=grant),
                            separators=(",", ":")).encode() + b"\n")
        def received(sock, data):
            for value in data:
                line.append(value)
                if len(line) > 4096:
                    raise ValueError("oversized header")
                if value != 10:
                    continue
                message = json.loads(line)
                line.clear()
                if not isinstance(message, dict):
                    raise ValueError("invalid header")
                with self._lock:
                    if not alive():
                        raise OSError("authorization ended")
                    if message.get("op") == "renewed":
                        self._endpoint(entry, message)
                    elif message.get("op") == "incoming":
                        session = message.get("session", "")
                        if not isinstance(session, str) or not re.fullmatch(r"[0-9a-f]{32}", session):
                            raise ValueError("invalid session")
                        if len(entry["sockets"]) < 64:
                            self._spawn(self._accept_seed, info_hash, entry, relay, session)
                    else:
                        raise ValueError("control refused")
        _pump((conn,), outgoing, lambda sock: True, received, alive, tick)

    def _accept_seed(self, info_hash, entry, relay, session):
        conn = local = None
        try:
            conn = self._tls(relay)
            self._track(info_hash, entry, conn)
            self._send(conn, dict(op="accept", session=session, grant=entry["grant"]))
            if self._json(conn).get("ok") is not True:
                return
            with self._lock:
                entry["used_relays"][relay["id"]] = dict(relay)
            local = socket.create_connection(("127.0.0.1", self.listen_port), timeout=TIMEOUT)
            self._splice(info_hash, entry, conn, local)
        except (OSError, ValueError):
            pass
        finally:
            with self._lock:
                for sock in (conn, local):
                    if sock:
                        _close(sock)
                        self._sockets.discard(sock)
                        entry["sockets"].discard(sock)

    def _live(self, entry):
        return (bool(entry["relays"]) and time.time() < min(entry["expiry"], entry["list_expiry"]))

    def _tls(self, relay):
        address = urlsplit("//" + relay["address"])
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE  # SPKI pin below is the trust anchor.
        context.minimum_version = ssl.TLSVersion.TLSv1_3
        raw = socket.create_connection((address.hostname, address.port), timeout=TIMEOUT)
        try:
            conn = context.wrap_socket(raw, server_hostname=address.hostname)
            cert = x509.load_der_x509_certificate(conn.getpeercert(binary_form=True))
            spki = cert.public_key().public_bytes(serialization.Encoding.DER,
                serialization.PublicFormat.SubjectPublicKeyInfo)
            if hashlib.sha256(spki).hexdigest() != relay["spki_sha256"].lower():
                _close(conn)
                raise OSError("pin mismatch")
            return conn
        except Exception:
            _close(raw)
            raise

    @staticmethod
    def _send(conn, request):
        data = json.dumps(request, separators=(",", ":")).encode() + b"\n"
        if len(data) > 4096:
            raise ValueError("oversized header")
        conn.sendall(data)

    @staticmethod
    def _json(conn):
        line = bytearray()
        while len(line) < 4096:
            b = _read(conn, 1)
            line.extend(b)
            if b == b"\n":
                result = json.loads(line)
                if not isinstance(result, dict):
                    raise ValueError("invalid header")
                return result
        raise ValueError("oversized header")

    def _track(self, info_hash, entry, *sockets):
        with self._lock:
            if entry is not None and (self._entries.get(info_hash) is not entry or not self._live(entry)):
                raise OSError("authorization ended")
            if self._stop.is_set():
                raise OSError("authorization ended")
            if len(self._sockets) + len(sockets) > MAX_CONNECTIONS:
                raise OSError("connection limit")
            if entry is not None:
                entry.setdefault("sockets", set()).update(sockets)
            self._sockets.update(sockets)

    def _splice(self, info_hash, entry, left, right):
        self._track(info_hash, entry, left, right)
        outgoing = {left: bytearray(), right: bytearray()}
        other = {left: right, right: left}
        def alive():
            with self._lock:
                return not self._stop.is_set() and (entry is None or
                    (self._entries.get(info_hash) is entry and self._live(entry)))
        try:
            _pump((left, right), outgoing,
                lambda sock: len(outgoing[other[sock]]) <= 65536 - 16384,
                lambda sock, data: outgoing[other[sock]].extend(data), alive)
        except (OSError, ValueError):
            pass
        finally:
            with self._lock:
                for sock in (left, right):
                    _close(sock)
                    self._sockets.discard(sock)
                    if entry is not None:
                        entry.get("sockets", set()).discard(sock)

    def remove(self, info_hash_hex: str) -> None:
        with self._lock:
            entry = self._entries.pop(info_hash_hex, None)
            for key in list(self._routes):
                if key[0] == info_hash_hex:
                    del self._routes[key]
            if entry:
                for sock in list(entry.get("sockets", ())):
                    _close(sock)
                    self._sockets.discard(sock)

    def _download(self, client, endpoint):
        handshake = _read(client, 68)
        if handshake[:20] != b"\x13BitTorrent protocol":
            return
        with self._lock:
            matches = [(h, e) for h, e in self._entries.items()
                       if bytes.fromhex(h)[:20] == handshake[28:48] and self._live(e)]
            if len(matches) != 1 or matches[0][1]["seed"]:
                return
            info_hash, entry = matches[0]
        for relay in entry["relays"]:
            conn = None
            try:
                if not self._live(entry):
                    return
                conn = self._tls(relay)
                self._track(info_hash, entry, conn)
                self._send(conn, dict(op="connect", endpoint=endpoint, grant=entry["grant"]))
                if self._json(conn).get("ok") is not True:
                    raise OSError("no route")
                conn.sendall(handshake)
                entry.setdefault("used_relays", {})[relay["id"]] = relay
                self._splice(info_hash, entry, client, conn)
                return
            except (OSError, ValueError):
                pass
            finally:
                if conn:
                    _close(conn)
                    with self._lock:
                        self._sockets.discard(conn)
                        entry.get("sockets", set()).discard(conn)

    def _http(self, client):
        body = b"d14:failure reason17:relay unavailablee"
        upstream = None
        try:
            client.settimeout(TIMEOUT)
            header = bytearray()
            while not header.endswith(b"\r\n\r\n") and len(header) < 8192:
                header.extend(_read(client, 1))
            if not header.endswith(b"\r\n\r\n"):
                raise ValueError("headers too large")
            lines = header.decode("ascii").split("\r\n")
            if len(lines) > 35:
                raise ValueError("too many headers")
            method, target, version = lines[0].split(" ")
            if method != "GET" or version not in ("HTTP/1.0", "HTTP/1.1") or not target.startswith("/announce/"):
                raise ValueError("invalid request")
            for line in lines[1:-2]:
                name, value = line.split(":", 1)
                if name.lower() in ("content-length", "transfer-encoding"):
                    raise ValueError("body not allowed")
            path, sep, query = target.partition("?")
            if not sep or len(query) > 4096 or len(query.split("&")) > 32:
                raise ValueError("invalid query")
            params = {}
            for field in query.split("&"):
                name, equal, value = field.partition("=")
                if not equal or re.search(r"%(?![a-fA-F0-9]{2})", field):
                    raise ValueError("invalid query")
                key = unquote_to_bytes(name.replace("+", " ")).decode("ascii")
                if key in params or key not in {"info_hash", "peer_id", "port", "uploaded", "downloaded", "left", "compact", "no_peer_id", "event", "numwant", "key", "supportcrypto", "redundant", "corrupt"}:
                    raise ValueError("invalid query")
                params[key] = unquote_to_bytes(value.replace("+", " "))
            left = params.get("left", b"")
            if not left.isdigit() or len(left) > 20:
                raise ValueError("invalid left")
            with self._lock:
                route = next((key for key, opaque in self._routes.items()
                              if path == "/announce/" + opaque), None)
                if route is None:
                    raise ValueError("unknown route")
                info_hash, original = route
                entry = self._entries.get(info_hash)
                if entry is None or entry["url"] != original:
                    raise ValueError("missing authorization")
                if "info_hash" in params and params["info_hash"] != bytes.fromhex(info_hash)[:20]:
                    raise ValueError("wrong hash")
                if entry["strict"] and not self._live(entry):
                    raise ValueError("expired authorization")
                if int(left) == 0:
                    if (not entry["seed"] or not entry.get("endpoint_token") or not self._live(entry)
                            or time.time() >= entry.get("endpoint_expiry", 0)):
                        raise ValueError("registration unavailable")
                    query += "&endpoint_token=" + quote(entry["endpoint_token"], safe="")
            p = urlsplit(original)
            cls = http.client.HTTPSConnection if p.scheme == "https" else http.client.HTTPConnection
            upstream = cls(p.hostname, p.port, timeout=TIMEOUT)
            upstream.request("GET", p.path + "?" + query, headers={"Accept": "application/x-bittorrent", "Connection": "close"})
            response = upstream.getresponse()
            if response.status != 200 or len(response.headers) > 32 or sum(len(k) + len(v) for k, v in response.headers.items()) > 8192:
                raise ValueError("tracker unavailable")
            received = response.read(1024 * 1024 + 1)
            if not received or len(received) > 1024 * 1024:
                raise ValueError("oversized response")
            body = _seed_reply(received) if int(left) == 0 else received
        except (OSError, ValueError, http.client.HTTPException, UnicodeError):
            pass
        finally:
            if upstream:
                upstream.close()
            try:
                client.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: application/x-bittorrent\r\nCache-Control: no-store\r\nConnection: close\r\nContent-Length: " + str(len(body)).encode() + b"\r\n\r\n" + body)
            except OSError:
                pass
            _close(client)
            with self._lock:
                self._sockets.discard(client)

    def _serve(self, listener=None, handler=None):
        listener = listener or self._listener
        handler = handler or self._socks
        while not self._stop.is_set():
            try:
                client, _ = listener.accept()
            except (OSError, socket.timeout):
                continue
            with self._lock:
                if len(self._sockets) >= MAX_CONNECTIONS or self._stop.is_set():
                    client.close()
                    continue
                self._sockets.add(client)
            if not self._spawn(handler, client):
                _close(client)
                with self._lock:
                    self._sockets.discard(client)

    def _socks(self, client):
        try:
            client.settimeout(TIMEOUT)
            version, count = _read(client, 2)
            methods = _read(client, count)
            if version != 5 or 0 not in methods:
                client.sendall(b"\x05\xff")
                return
            client.sendall(b"\x05\x00")
            version, command, reserved, atyp = _read(client, 4)
            address, port = "", 0
            if atyp == 1:
                address = socket.inet_ntoa(_read(client, 4))
            elif atyp == 3:
                address = _read(client, _read(client, 1)[0]).decode("ascii")
            elif atyp == 4:
                address = socket.inet_ntop(socket.AF_INET6, _read(client, 16))
            if atyp in (1, 3, 4):
                port = int.from_bytes(_read(client, 2), "big")
            virtual = atyp == 1 and ipaddress.ip_address(address) in VIRTUAL
            webseed = bool(self._webseed_host) and port == self._webseed_port and (
                address == self._webseed_host or address in self._webseed_addresses)
            allowed = version == 5 and command == 1 and reserved == 0 and port > 0 and (virtual or webseed)
            client.sendall(b"\x05" + (b"\x00" if allowed else b"\x02") + b"\x00\x01" + b"\x00" * 6)
            if allowed and webseed:
                upstream = None
                try:
                    # Dial only the configured origin, never resolve client input.
                    upstream = socket.create_connection((self._webseed_host, self._webseed_port), timeout=TIMEOUT)
                    self._splice(None, None, client, upstream)
                finally:
                    if upstream:
                        _close(upstream)
            elif allowed:
                self._download(client, address + ":" + str(port))
        except (OSError, ValueError):
            pass
        finally:
            _close(client)
            with self._lock:
                self._sockets.discard(client)

    def close(self):
        self._stop.set()
        _close(self._listener)
        _close(self._http_listener)
        with self._lock:
            for sock in list(self._sockets):
                _close(sock)
        self._thread.join(timeout=1)
