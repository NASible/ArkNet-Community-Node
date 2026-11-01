import http.server
import json
import threading
import unittest
import urllib.parse

from scripts import global_login as gl


class FakeNodeAndCore(http.server.BaseHTTPRequestHandler):
    """One server plays both the Node member API and Core's /node-login."""
    callback = ""
    seen_cookies: list = []

    def log_message(self, format, *args):  # noqa: A002 - BaseHTTPRequestHandler signature
        pass

    def _json(self, status, body, cookies=()):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        for cookie in cookies:
            self.send_header("Set-Cookie", cookie)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        page = b'<form><input name="password"></form>'
        self.send_response(200)
        self.send_header("Content-Length", str(len(page)))
        self.end_headers()
        self.wfile.write(page)

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        FakeNodeAndCore.seen_cookies.append(self.headers.get("Cookie"))
        if self.path == "/api/v1/global-users/login":
            origin = f"http://127.0.0.1:{self.server.server_address[1]}"
            self._json(200, {"mode": "redirect", "authorize_url": origin + "/node-login?node_id=n&nonce=x&challenge=y&redirect_uri=cb"},
                       ["__Host-arknet_global_login=STATE-VALUE-0123456789; Path=/; Max-Age=300; HttpOnly; Secure; SameSite=Lax"])
        elif self.path == "/node-login":
            form = urllib.parse.parse_qs(body.decode())
            ok = form.get("password") == ["pw"] and self.headers.get("Sec-Fetch-Site") == "same-origin"
            self.send_response(303 if ok else 401)
            if ok:
                self.send_header("Location", FakeNodeAndCore.callback + "#assertion=ASSERTION-PAYLOAD.SIGNATURE-0123456789")
            self.send_header("Content-Length", "0")
            self.end_headers()
        elif self.path == "/api/v1/global-users/login/complete":
            ok = json.loads(body) == {"assertion": "ASSERTION-PAYLOAD.SIGNATURE-0123456789"}
            self._json(201 if ok else 401, {"kind": "GLOBAL", "token": "SESSION-TOKEN-0123456789"},
                       ["__Host-arknet_global_login=; Path=/; Max-Age=0", "__Host-arknet_global_device=DEVICE-0123456789abcdef; Path=/; Max-Age=60"])


class GlobalLoginHarnessTests(unittest.TestCase):
    def setUp(self):
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), FakeNodeAndCore)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.server.server_port}"
        FakeNodeAndCore.callback = self.base + "/login/callback"
        FakeNodeAndCore.seen_cookies = []

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def test_login_follows_core_redirect_and_keeps_cookies(self):
        browser = gl.Browser()
        session, assertion = gl.login(browser, self.base, FakeNodeAndCore.callback, "alice", "pw")
        self.assertEqual(session["token"], "SESSION-TOKEN-0123456789")
        self.assertEqual(assertion, "ASSERTION-PAYLOAD.SIGNATURE-0123456789")
        # The login-state cookie was sent back and then cleared; the device cookie remains.
        self.assertIn("__Host-arknet_global_login=STATE-VALUE-0123456789", FakeNodeAndCore.seen_cookies[-1])
        self.assertEqual(browser.cookie_values(), ["DEVICE-0123456789abcdef"])

    def test_wrong_password_fails_without_assertion(self):
        with self.assertRaises(AssertionError):
            gl.login(gl.Browser(), self.base, FakeNodeAndCore.callback, "alice", "wrong")

    def test_relay_records_everything_the_node_receives(self):
        relay = gl.RecordingRelay("127.0.0.1", self.server.server_port)
        try:
            gl.login(gl.Browser(), relay.base, FakeNodeAndCore.callback, "alice", "pw")
        finally:
            relay.close()
        candidates = relay.candidates()
        for value in ("STATE-VALUE-0123456789", "ASSERTION-PAYLOAD.SIGNATURE-0123456789", "SIGNATURE-0123456789"):
            self.assertIn(value, candidates)


if __name__ == "__main__":
    unittest.main()
