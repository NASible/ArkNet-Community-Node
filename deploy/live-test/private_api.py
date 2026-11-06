"""Private API helpers: never expose response bodies/tokens in errors."""
import contextlib
import json
import socket
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from ops import HERE, MACHINES, load_secrets, save_secrets, ssh
sys.path.insert(0, str(HERE.parent.parent))
from scripts import operator_login  # noqa: E402

TLS = ssl.create_default_context(cafile=str(HERE / 'nodenet-root-ca.crt'))


def request(url, method='GET', body=None, headers=None, expected=(200, 201, 204)):
    data = json.dumps(body).encode() if body is not None else None
    h = {'Content-Type': 'application/json', **(headers or {})}
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, context=TLS, timeout=30) as response:
            status, value = response.status, response.read()
    except urllib.error.HTTPError as error:
        status, value = error.code, error.read()
    if status not in expected:
        raise RuntimeError(f'{method} API request failed: HTTP {status}; response withheld')
    return status, json.loads(value) if value else None


def _raw(url, method='GET', body=None, headers=None):
    """(status, headers, body) for operator_login; never raises on HTTP errors."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, headers={'Content-Type': 'application/json', **(headers or {})}, method=method)
    try:
        with urllib.request.urlopen(req, context=TLS, timeout=30) as response:
            return response.status, dict(response.headers.items()), response.read()
    except urllib.error.HTTPError as error:
        return error.code, dict(error.headers.items()), error.read()


_operator_cookies = {}


def operator_headers(host, base=None):
    """Cookie headers for a Node operator session (Prompt 45B management auth).

    `base` defaults to the Node's management HTTPS host. initialize-node.py
    runs before ingress is activated and passes a loopback tunnel to the
    Agent's member listener instead: setup and sign-in are public routes,
    served only there (Prompt 45C). The cookie then authorizes management.

    First use bootstraps the Node's operator with a one-time setup code read
    from the Agent container (held in memory only); its password, TOTP secret
    and recovery codes are stored only in secrets/runtime.json (0600). Later
    uses sign in with password + TOTP. Nothing secret is printed.
    """
    if host in _operator_cookies:
        return {'Cookie': operator_login.COOKIE + '=' + _operator_cookies[host]}
    state = load_secrets()
    local = state['machines'][host]
    base = base or 'https://node-' + host[-1] + '.nodenet.arknet.media'
    operator = local.get('operator')
    if not operator:
        output = ssh(MACHINES[host], 'cd /opt/arknet && sudo docker compose --env-file runtime.env -f compose.json '
                     'exec -T arknet-agent arknet-agent operator setup-code --if-none').decode()
        code = operator_login.setup_code(output)
        created = operator_login.bootstrap(base, code, _raw, username='operator-' + host[-1])
        local['operator'] = {'username': created.username, 'password': created.password,
                             'totp_secret': created.totp_secret, 'recovery_codes': created.recovery_codes}
        save_secrets(state)
        _operator_cookies[host] = created.cookie
    else:
        for attempt in range(2):
            status, headers, _ = _raw(base + '/api/v1/operator/sessions', 'POST', {
                'username': operator['username'], 'password': operator['password'],
                'totp': operator_login.totp(operator['totp_secret'])})
            if status in (200, 201):
                _operator_cookies[host] = operator_login.session_cookie(headers)
                break
            if attempt == 0 and status == 401:
                # A TOTP step is single use: wait for the next one once.
                time.sleep(31 - time.time() % 30)
                continue
            raise RuntimeError(host + ': operator sign-in failed: HTTP ' + str(status) + '; response withheld')
    return {'Cookie': operator_login.COOKIE + '=' + _operator_cookies[host]}


def wait(description, callback, timeout=180):
    deadline = time.monotonic() + timeout
    last_error = None
    while time.monotonic() < deadline:
        try:
            value = callback()
            if value:
                return value
        except (RuntimeError, OSError, urllib.error.URLError) as error:
            last_error = type(error).__name__
        time.sleep(3)
    raise RuntimeError('Timed out: ' + description + '; last error type=' + str(last_error))


@contextlib.contextmanager
def tunnel(machine, remote_port):
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    args = ['ssh', '-i', str(Path.home() / '.ssh/arknet-live-test'),
            '-o', 'IdentitiesOnly=yes', '-o', 'BatchMode=yes',
            '-o', 'StrictHostKeyChecking=yes', '-o', 'ExitOnForwardFailure=yes',
            '-o', 'UserKnownHostsFile=' + str(Path.home() / '.ssh/arknet-live-test-known_hosts'),
            '-o', 'HostKeyAlias=' + machine['lan_ip'], '-N',
            '-L', f'127.0.0.1:{port}:127.0.0.1:{remote_port}',
            'arknet@' + machine['tailscale_ip']]
    process = subprocess.Popen(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        def ready():
            if process.poll() is not None:
                raise RuntimeError('SSH tunnel exited')
            with socket.create_connection(('127.0.0.1', port), timeout=1):
                return True
        wait('SSH tunnel', ready, timeout=15)
        yield f'http://127.0.0.1:{port}'
    finally:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
