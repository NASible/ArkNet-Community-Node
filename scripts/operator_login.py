"""Operator sign-in for acceptance harnesses (Prompt 45B).

Management routes need an operator session. A harness obtains one the way an
operator does: a one-time setup code from `arknet-agent operator setup-code`
inside the Agent container, then the setup and confirm API calls with a TOTP
code. Everything secret is returned in `secrets` so harnesses can redact it.
"""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import re
import secrets as random
import struct
import time
from dataclasses import dataclass, field
from typing import Callable

COOKIE = "__Host-arknet_operator"
CODE = re.compile(r"[A-Z2-7]{4}(?:-[A-Z2-7]{4}){7}")


def setup_code(output: str) -> str:
    match = CODE.search(output or "")
    if not match:
        raise AssertionError("arknet-agent printed no operator setup code")
    return match.group(0)


def redact_codes(text: str) -> str:
    return CODE.sub("[SETUP-CODE]", text)


def totp(secret_b32: str, at: float | None = None, offset: int = 0) -> str:
    key = base64.b32decode(secret_b32 + "=" * (-len(secret_b32) % 8))
    step = int(time.time() if at is None else at) // 30 + offset
    mac = hmac.new(key, struct.pack(">Q", step), hashlib.sha1).digest()
    o = mac[-1] & 0x0F
    return "%06d" % ((struct.unpack(">I", mac[o:o + 4])[0] & 0x7FFFFFFF) % 1_000_000)


def session_cookie(headers: dict) -> str:
    """The session token from a Set-Cookie header (any header-name case)."""
    for name, value in headers.items():
        match = re.search(re.escape(COOKIE) + r"=([^;,\s]+)", value) if name.lower() == "set-cookie" else None
        if match:
            return match.group(1)
    raise AssertionError("the Agent set no operator session cookie")


@dataclass
class Operator:
    username: str
    password: str
    totp_secret: str
    cookie: str
    recovery_codes: list[str]
    secrets: list[str] = field(default_factory=list)

    @property
    def headers(self) -> dict:
        return {"Cookie": f"{COOKIE}={self.cookie}"}


def bootstrap(base: str, code: str, request: Callable, username: str = "harness-operator") -> Operator:
    """Create an operator with a setup or recovery code and return its session.

    `request(url, method, body, headers)` returns (status, headers, body_bytes),
    like the harnesses' shared request helper.
    """
    password = "harness " + random.token_urlsafe(24)
    status, _, body = request(base + "/api/v1/operator/setup", "POST",
                              {"setup_code": code, "username": username, "password": password}, None)
    if status != 201:
        raise AssertionError(f"operator setup: HTTP {status}")
    started = json.loads(body)
    status, headers, body = request(base + "/api/v1/operator/setup/confirm", "POST",
                                    {"enrollment_token": started["enrollment_token"], "totp": totp(started["totp_secret"])}, None)
    if status != 201:
        raise AssertionError(f"operator setup confirm: HTTP {status}")
    confirmed = json.loads(body)
    cookie = session_cookie(headers)
    operator = Operator(username, password, started["totp_secret"], cookie, confirmed["recovery_codes"])
    operator.secrets = [code, code.replace("-", ""), password, started["totp_secret"], started["enrollment_token"], cookie,
                        *confirmed["recovery_codes"]]
    return operator
