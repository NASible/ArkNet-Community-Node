"""Mint per-run, least-privilege Core administrator tokens for harnesses.

Tokens come only from the shell CLI inside the Core container
(`arknet-core admin create-token`); there is no HTTP route that mints them.
"""
import json

TOKEN_PREFIX = "arknet_adm_"
PERMISSIONS = {"nodes.decide", "users.admin", "media.admin", "relays.read"}


def mint_admin_token(compose, name, *permissions, ttl="2h"):
    """Return a bearer token holding exactly `permissions`.

    `compose(*args, capture=True)` must run `docker compose ... <args>` for the
    Core project and return stdout.
    """
    if not permissions or not set(permissions) <= PERMISSIONS or len(set(permissions)) != len(permissions):
        raise ValueError("name one or more known permissions, without repeats")
    args = ["create-token", "--name", name]
    for permission in permissions:
        args += ["--permission", permission]
    args += ["--ttl", ttl]
    result = json.loads(compose("exec", "-T", "arknet-core", "arknet-core", "admin", *args, capture=True))
    token = result.get("token", "")
    assert token.startswith(TOKEN_PREFIX) and len(token) == len(TOKEN_PREFIX) + 43, "CLI did not return a token"
    assert sorted(result.get("permissions", [])) == sorted(permissions), "CLI granted different permissions"
    return token


class ScopedAdmin:
    """A least-privilege admin credential minted on first use (after Core is up)."""

    def __init__(self, compose, name, *permissions, ttl="2h"):
        self._args = (compose, name, *permissions)
        self._ttl = ttl
        self._token = None

    @property
    def token(self):
        if self._token is None:
            self._token = mint_admin_token(*self._args, ttl=self._ttl)
        return self._token

    def headers(self):
        return {"Authorization": "Bearer " + self.token}

    def secrets(self):
        """Minted values, for "never appears in logs" checks."""
        return [] if self._token is None else [self._token]
