"""Browser-equivalent Core-bound global login for test harnesses (Prompt 46B).

The flow matches docs/global-login-assertions.md: the Node starts a login and
sets an HttpOnly login-state cookie, the browser signs in on Core's own
``/node-login`` page, Core answers ``303`` to the Node's registered callback
with the assertion in the URL fragment, and the browser posts the assertion
back to the Node. Standard library only; secrets are never printed.

``RecordingRelay`` is the "malicious Node" harness: a TCP relay in front of a
Node's member listener that records every byte the Agent receives.
"""
from __future__ import annotations

import http.cookies
import json
import re
import socket
import threading
import urllib.error
import urllib.parse
import urllib.request

from scripts.rate_limit_retry import retry_429


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):  # noqa: D401 - urllib hook
        return None


class Browser:
    """Minimal cookie-keeping client; cookies are scoped per origin."""

    def __init__(self, context=None):
        handlers: list[urllib.request.BaseHandler] = [_NoRedirect()]
        if context is not None:
            handlers.append(urllib.request.HTTPSHandler(context=context))
        self.opener = urllib.request.build_opener(*handlers)
        self.cookies: dict[str, dict[str, str]] = {}

    @staticmethod
    def _origin(url):
        parts = urllib.parse.urlsplit(url)
        return f"{parts.scheme}://{parts.netloc}"

    def request(self, url, method="GET", body=None, form=None, headers=None, timeout=30):
        origin = self._origin(url)
        merged = {"Sec-Fetch-Site": "same-origin", **(headers or {})}
        data = None
        if form is not None:
            data = urllib.parse.urlencode(form).encode()
            merged.setdefault("Content-Type", "application/x-www-form-urlencoded")
            merged.setdefault("Origin", origin)
        elif body is not None:
            data = json.dumps(body).encode()
            merged.setdefault("Content-Type", "application/json")
        jar = self.cookies.get(origin, {})
        if jar:
            merged["Cookie"] = "; ".join(f"{k}={v}" for k, v in jar.items())
        req = urllib.request.Request(url, data=data, method=method, headers=merged)
        try:
            response = self.opener.open(req, timeout=timeout)
            status, response_headers, raw = response.status, response.headers, response.read()
        except urllib.error.HTTPError as error:
            status, response_headers, raw = error.code, error.headers, error.read()
        for header in response_headers.get_all("Set-Cookie") or []:
            cookie = http.cookies.SimpleCookie()
            cookie.load(header)
            for name, morsel in cookie.items():
                if morsel["max-age"] and int(morsel["max-age"]) <= 0:
                    self.cookies.setdefault(origin, {}).pop(name, None)
                else:
                    self.cookies.setdefault(origin, {})[name] = morsel.value
        try:
            decoded = json.loads(raw) if raw else None
        except ValueError:
            decoded = None
        return status, response_headers, decoded, raw

    def cookie_values(self):
        return [v for jar in self.cookies.values() for v in jar.values()]


def start(browser, member_base, callback_url):
    status, _, data, _ = browser.request(member_base + "/api/v1/global-users/login", "POST", {"callback_url": callback_url})
    return status, data


def core_sign_in(browser, authorize_url, username, password):
    """Sign in on Core's page; returns (status, assertion or None, location)."""
    status, _, _, page = browser.request(authorize_url)
    if status != 200 or b'name="password"' not in page:
        return status, None, None
    query = urllib.parse.parse_qs(urllib.parse.urlsplit(authorize_url).query)
    form = {name: values[0] for name, values in query.items()}
    form.update(username=username, password=password)
    core = Browser._origin(authorize_url)
    # Browser and API logins share Core's source budget. Re-POST this exact form
    # only after a 429; never restart the flow or retry authoritative denials.
    status, headers, _, _ = retry_429(lambda: browser.request(core + "/node-login", "POST", form=form))
    location = headers.get("Location") if status == 303 else None
    assertion = None
    if location and "#assertion=" in location:
        assertion = location.split("#assertion=", 1)[1]
    return status, assertion, location


def complete(browser, member_base, assertion):
    status, _, data, _ = browser.request(member_base + "/api/v1/global-users/login/complete", "POST", {"assertion": assertion})
    return status, data


def login(browser, member_base, callback_url, username, password):
    """Full online login; returns (session, assertion). Raises AssertionError."""
    status, started = start(browser, member_base, callback_url)
    assert status == 200 and started.get("mode") == "redirect", f"login start: HTTP {status}"
    status, assertion, location = core_sign_in(browser, started["authorize_url"], username, password)
    assert status == 303 and assertion, f"Core sign-in: HTTP {status}"
    assert location and location.startswith(callback_url + "#"), "Core redirected somewhere other than the registered callback"
    status, session = complete(browser, member_base, assertion)
    assert status == 201 and session.get("kind") == "GLOBAL", f"login complete: HTTP {status}"
    return session, assertion


class RecordingRelay:
    """TCP relay that records every byte sent to the target (the Agent)."""

    def __init__(self, target_host, target_port):
        self.target = (target_host, target_port)
        self.received = bytearray()
        self.lock = threading.Lock()
        self.server = socket.socket()
        self.server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.server.bind(("127.0.0.1", 0))
        self.server.listen(16)
        self.port = self.server.getsockname()[1]
        self.closed = False
        threading.Thread(target=self._accept, daemon=True).start()

    @property
    def base(self):
        return f"http://127.0.0.1:{self.port}"

    def _accept(self):
        while not self.closed:
            try:
                client, _ = self.server.accept()
            except OSError:
                return
            upstream = socket.create_connection(self.target, timeout=30)
            threading.Thread(target=self._pipe, args=(client, upstream, True), daemon=True).start()
            threading.Thread(target=self._pipe, args=(upstream, client, False), daemon=True).start()

    def _pipe(self, source, sink, record):
        try:
            while True:
                chunk = source.recv(65536)
                if not chunk:
                    break
                if record:
                    with self.lock:
                        self.received.extend(chunk)
                sink.sendall(chunk)
        except OSError:
            pass
        finally:
            for s in (source, sink):
                try:
                    s.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
                s.close()

    def close(self):
        self.closed = True
        self.server.close()

    def candidates(self):
        """Every token-like value the Agent received (headers, cookies, bodies)."""
        with self.lock:
            text = self.received.decode("latin-1")
        values = set(re.findall(r"[A-Za-z0-9_\-.~+/=]{16,}", text))
        for value in list(values):
            values.update(part for part in re.split(r"[.=]", value) if len(part) >= 16)
        return sorted(values)
