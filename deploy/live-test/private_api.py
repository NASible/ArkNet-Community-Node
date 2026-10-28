"""Private API helpers: never expose response bodies/tokens in errors."""
import contextlib
import json
import socket
import ssl
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path
from ops import HERE

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
