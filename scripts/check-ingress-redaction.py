#!/usr/bin/env python3
"""Real-container TLS ingress sentinel acceptance check (no host trust changes).

Raw child output is captured privately, never replayed on failure. Test CA, keys,
logs, and grants live only in a disposable HOME scratch directory. No stubs.
"""
import urllib.parse
import collections
import json
import os
from pathlib import Path
import subprocess
import ssl
import shutil

OPENSSL = (os.environ.get('ARKNET_TEST_OPENSSL') or
           next((p for p in ('/opt/homebrew/opt/openssl@3/bin/openssl',
                             '/usr/local/opt/openssl@3/bin/openssl') if Path(p).is_file()), None) or
           shutil.which('openssl'))


def make_tls(root):
    # No private material appears in command arguments, only file paths.
    run([OPENSSL, 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
         '-subj', '/CN=ArkNet disposable ingress CA', '-keyout', str(root / 'ca.key'),
         '-out', str(root / 'ca.pem')])
    run([OPENSSL, 'req', '-new', '-newkey', 'rsa:2048', '-nodes',
         '-subj', '/CN=localhost', '-keyout', str(root / 'key.pem'),
         '-out', str(root / 'request.pem')])
    (root / 'extensions').write_text('subjectAltName=DNS:localhost\nextendedKeyUsage=serverAuth\n')
    run([OPENSSL, 'x509', '-req', '-in', str(root / 'request.pem'), '-CA', str(root / 'ca.pem'),
         '-CAkey', str(root / 'ca.key'), '-CAcreateserial', '-days', '1',
         '-extfile', str(root / 'extensions'), '-out', str(root / 'cert.pem')])
    for name in ('ca.key', 'key.pem'):
        (root / name).chmod(0o600)


def tls_context(root):
    return ssl.create_default_context(cafile=str(root / 'ca.pem'))


class Stack:
    def __init__(self, name, scratch, environment, files):
        self.name = name
        self.files = files
        self.envfile = scratch / (name + '.env')
        self.envfile.write_bytes(b'')
        self.env = {key: value for key, value in os.environ.items()
                    if key in ('PATH', 'HOME', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_CONFIG',
                               'SSH_AUTH_SOCK', 'TMPDIR')}
        self.env.update(environment)
        self.env['COMPOSE_DISABLE_ENV_FILE'] = 'true'

    def command(self, *args):
        command = ['docker', 'compose', '--env-file', str(self.envfile), '--project-name', self.name]
        for path in self.files:
            command += ['-f', str(path)]
        return command + list(args)

    def call(self, *args, timeout=120, check=True):
        return run(self.command(*args), env=self.env, timeout=timeout, check=check)

    def container(self, service):
        value = self.call('ps', '-aq', service).stdout.decode().strip()
        if not value or '\n' in value:
            raise CheckFailure('missing unique container: ' + service)
        return value


ROOT = Path(__file__).resolve().parent.parent


def run(argv, *, timeout=60, env=None, check=True, input_data=None):
    try:
        result = subprocess.run(argv, cwd=ROOT, env=env, input=input_data,
                                capture_output=True, timeout=timeout)
    except (OSError, subprocess.TimeoutExpired):
        raise CheckFailure("child process unavailable or timed out") from None
    if check and result.returncode:
        raise CheckFailure("child process failed")
    return result


def require_access(data, expected):
    found = collections.Counter()
    for line in data.splitlines():
        try:
            record = json.loads(line)
            found[(record['route'], int(record['status']))] += 1
        except (ValueError, KeyError, TypeError):
            continue
    if collections.Counter(expected) - found:
        raise CheckFailure("missing ingress access route/outcome evidence")



class CheckFailure(RuntimeError):
    """An intentionally allowlisted, credential-free diagnostic."""


def scan_sink(name, data, secrets):
    """Detect raw, mixed-case percent encoded, and nested URL encoded secrets."""
    values = [data]
    for _ in range(3):
        values.append(urllib.parse.unquote_to_bytes(values[-1].replace(b"+", b" ")))
    for secret in secrets:
        raw = secret.encode()
        forms = (raw, urllib.parse.quote(secret, safe="").encode(),
                 urllib.parse.quote_plus(secret, safe="").encode())
        if any(form in value for form in forms for value in values):
            raise CheckFailure("sensitive data in sink: " + name)


import http.client
import importlib.util
import secrets
import socket
import tempfile
import time
import uuid
import signal


def runtime_expired(signum, frame):
    raise CheckFailure('bounded acceptance runtime expired')


def check(condition, message):
    if not condition:
        raise CheckFailure(message)


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def request(port, path, *, context=None, headers=None, method='GET'):
    connection = (http.client.HTTPSConnection('localhost', port, context=context, timeout=40)
                  if context else http.client.HTTPConnection('127.0.0.1', port, timeout=40))
    try:
        connection.request(method, path, headers=headers or {})
        response = connection.getresponse()
        return response.status, dict(response.getheaders()), response.read()
    except (OSError, http.client.HTTPException, ValueError):
        raise CheckFailure('HTTP transport failed') from None
    finally:
        connection.close()


def wait_for(probe, label, seconds=60):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if probe():
                return
        except CheckFailure:
            pass
        time.sleep(0.25)
    raise CheckFailure('readiness failed: ' + label)


def capture_logs(stack, service, root, suffix=''):
    result = run(['docker', 'logs', stack.container(service)])
    # Retain the exact two raw streams privately before any decoding/scanning.
    for stream in ('stdout', 'stderr'):
        path = root / (service + suffix + '.' + stream)
        with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), 'wb') as handle:
            handle.write(getattr(result, stream))
    return result.stdout + b'\n' + result.stderr


def private_override(root, name, services, volumes=None):
    path = root / (name + '.json')
    document = {'services': services}
    if volumes:
        document['volumes'] = volumes
    path.write_text(json.dumps(document))
    path.chmod(0o600)
    return path


def inspect_container(stack, service):
    return json.loads(run(['docker', 'inspect', stack.container(service)]).stdout)[0]


def main():
    scratch = Path.home() / '.hermes/cache/scratch'
    scratch.mkdir(parents=True, exist_ok=True)
    prefix = 'arknet-ingress-test-' + uuid.uuid4().hex[:12]
    root = Path(tempfile.mkdtemp(prefix=prefix + '-', dir=scratch))
    root.chmod(0o755)  # Core's unprivileged UID must traverse bind sources.
    stacks, images = [], []
    stage = 'initialization'
    cleanup_ok = True
    previous_alarm = signal.signal(signal.SIGALRM, runtime_expired)
    signal.alarm(900)
    try:
        make_tls(root)
        context = tls_context(root)
        tlsdir = root / 'tls'
        tlsdir.mkdir(mode=0o755)
        for name in ('cert.pem', 'key.pem'):
            shutil.move(str(root / name), str(tlsdir / name))
        webroot = root / 'media'
        webroot.mkdir(mode=0o755)
        spec = importlib.util.spec_from_file_location('tracker_helper', ROOT / 'scripts/check-private-tracker-stack.py')
        assert spec is not None and spec.loader is not None
        helper_module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(helper_module)
        helper = root / 'keys.go'
        helper.write_text(helper_module.KEY_HELPER)
        relay = root / 'relay'
        relay.mkdir()
        # Canonical five-minute v2 grants and file-backed Ed25519 private key.
        issued = json.loads(run(['go', 'run', str(helper), str(root)], timeout=120).stdout)
        run(['go', 'run', str(helper), str(relay)], timeout=120)
        # Keep signing material private; the fixture helper below assigns the
        # shipped Core UID before it is mounted into the running container.
        (root / 'private.key').chmod(0o600)
        token = issued['download_token']
        media_id = 'media_' + secrets.token_hex(32)
        marker = 'arknet-sentinel-' + secrets.token_hex(16)
        secret_values = [*issued.values(), media_id, marker]
        cp, tp, ap, mp, up, uhp = (free_port() for _ in range(6))
        core_environment = {
            'POSTGRES_PASSWORD': secrets.token_hex(32),
            'ARKNET_TRACKER_SIGNING_KEY_FILE': str(root / 'private.key'),
            'ARKNET_TRACKER_VERIFY_KEY_FILE': str(root / 'public.key'),
            'ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE': str(relay / 'public.key'),
            'ARKNET_TRACKER_ANNOUNCE_URL': f'https://localhost:{tp}',
            'ARKNET_CORE_WEB_SEED_ORIGIN': f'https://localhost:{cp}',
            'ARKNET_CORE_WEB_SEED_SOURCE': str(webroot),
            'ARKNET_INGRESS_TLS_CERT_FILE': str(tlsdir / 'cert.pem'),
            'ARKNET_INGRESS_TLS_KEY_FILE': str(tlsdir / 'key.pem'),
            'ARKNET_CORE_HTTPS_BIND': f'127.0.0.1:{cp}',
            'ARKNET_TRACKER_HTTPS_BIND': f'127.0.0.1:{tp}',
        }
        core_images: dict[str, dict] = {s: {'image': prefix + '-' + s} for s in ('arknet-core', 'arknet-tracker')}
        core_images['arknet-core'].update({
            'environment': {'ARKNET_TRACKER_SIGNING_KEY_FILE': '/test-signing/private.key'},
            'volumes': [{'type': 'volume', 'source': 'signing-fixture', 'target': '/test-signing', 'read_only': True}]})
        node_images: dict[str, dict] = {s: {'image': prefix + '-' + s} for s in ('arknet-agent', 'arknet-ui')}
        node_images['arknet-ui']['environment'] = {
            'DEBUG': 'vite:proxy', 'NODE_DEBUG': 'http', 'NODE_DEBUG_NATIVE': 'http'}
        images.extend(v['image'] for v in [*core_images.values(), *node_images.values()])
        core = Stack(prefix + '-core', root, core_environment,
                     [ROOT / 'deploy/core' / f for f in ('compose.yaml', 'compose.tracker.yaml',
                       'compose.web-seed.yaml', 'compose.ingress.yaml')] +
                     [private_override(root, 'core-images', core_images,
                                       {'signing-fixture': {'external': True, 'name': prefix + '-signing'}})])
        node = Stack(prefix + '-node', root, {
            'POSTGRES_PASSWORD': secrets.token_hex(32), 'ARKNET_TLS_DIR': str(tlsdir),
            'ARKNET_AGENT_PORT': str(ap), 'ARKNET_MEMBER_PORT': str(mp),
            'ARKNET_UI_PORT': str(up), 'ARKNET_UI_HTTPS_BIND': f'127.0.0.1:{uhp}'},
            [ROOT / 'deploy/node/compose.yaml', private_override(root, 'node-images', node_images)])
        stacks.extend((core, node))
        stage = 'compose configuration'
        config = json.loads(core.call('config', '--format', 'json').stdout)
        for service in ('arknet-core', 'arknet-tracker'):
            check(not config['services'][service].get('ports'), 'direct upstream port rendered published')
        node.call('config', '--format', 'json')
        stage = 'Core/tracker image build'
        print('SETUP: building isolated shipped Core/tracker/Agent/UI stacks', flush=True)
        core.call('build', 'arknet-core', 'arknet-tracker', timeout=600)
        stage = 'Agent/UI image build'
        node.call('build', 'arknet-agent', 'arknet-ui', timeout=600)
        # Issue only after image builds, so cold builds cannot expire the grant.
        issued = json.loads(run(['go', 'run', str(helper), str(root)], timeout=120).stdout)
        (root / 'private.key').chmod(0o600)
        token = issued['download_token']
        secret_values = [*issued.values(), media_id, marker]
        stage = 'disposable TLS key ownership'
        # The shipped ingress drops DAC_OVERRIDE; its root master can read a
        # mode-0600 key only when root owns it (as on the deployed host). Chown
        # this exact scratch key through Docker, never a live certificate file.
        run(['docker', 'run', '--rm', '--name', prefix + '-tls-owner', '--network', 'none', '--user', '0:0',
             '--entrypoint', 'sh', '--mount', 'type=bind,src=' + str(tlsdir / 'key.pem') + ',dst=/test-key',
             config['services']['arknet-ingress']['image'], '-c',
             'chown 0:0 /test-key && chmod 600 /test-key && '
             'test "$(stat -c %u:%g:%a /test-key)" = 0:0:600'], timeout=120)
        # macOS bind mounts can ignore non-root chown. Use a dedicated volume
        # for Core's UID-owned signing fixture; send key bytes only via stdin.
        run(['docker', 'volume', 'create', prefix + '-signing'])
        run(['docker', 'run', '--rm', '-i', '--name', prefix + '-signing-owner', '--network', 'none',
             '--entrypoint', 'sh', '--mount', 'type=volume,src=' + prefix + '-signing,dst=/fixture',
             config['services']['arknet-ingress']['image'], '-c',
             'umask 077; cat > /fixture/private.key && chown 10001:10001 /fixture/private.key && '
             'chmod 600 /fixture/private.key && test "$(stat -c %u:%g:%a /fixture/private.key)" = 10001:10001:600'],
            input_data=(root / 'private.key').read_bytes(), timeout=60)
        stage = 'Core/tracker/ingress startup'
        core.call('up', '-d', '--wait', '--wait-timeout', '120', timeout=180)
        stage = 'Agent/UI startup'
        node.call('up', '-d', '--wait', '--wait-timeout', '120',
                  'postgres', 'arknet-agent', 'arknet-ui', timeout=180)
        upstream_networks = {}
        for service in ('arknet-core', 'arknet-tracker'):
            state = inspect_container(core, service)
            upstream_networks[service] = next(iter(state['NetworkSettings']['Networks']))
            check(not state['HostConfig'].get('PortBindings'), 'direct upstream port requested published')
            check(not any(state['NetworkSettings']['Ports'].values()), 'direct upstream port actually published')
        stage = 'TLS verification'
        for port in (cp, tp):
            check(request(port, '/health', context=context)[0] == 200, 'TLS health failed')
            for hostname, trust in [('127.0.0.1', context), ('localhost', ssl.create_default_context())]:
                try:
                    with socket.create_connection(('127.0.0.1', port), timeout=5) as sock:
                        with trust.wrap_socket(sock, server_hostname=hostname):
                            pass
                except ssl.SSLCertVerificationError:
                    pass
                else:
                    raise CheckFailure('TLS accepted wrong hostname or untrusted CA')
        headers = {'Referer': 'https://localhost/' + marker + '/' + token,
                   'User-Agent': marker + '/' + token, 'X-Arknet-Sentinel': marker + '/' + token,
                   'Authorization': 'Bearer ' + token}
        expected = []
        labels = {'web_seed': '/api/web-seed/v1/media/[REDACTED]',
                  'torrent': '/api/torrent/v1/media/[REDACTED]',
                  'tracker': '/announce/[REDACTED]', 'other': '[REDACTED]',
                  'api': '/api/[REDACTED]'}

        def probe(port, path, label, status=None, method='GET'):
            actual, response_headers, body = request(port, path, context=context, headers=headers, method=method)
            # http.client never follows redirects; check Location independently.
            scan_sink('response-location', response_headers.get('Location', '').encode(), secret_values)
            check(not 300 <= actual < 400, 'unexpected ingress redirect')
            if status is not None:
                check(actual == status, 'unexpected status: ' + label + ' expected ' + str(status) + ' received ' + str(actual))
            expected.append((labels[label], actual))
            return actual, body

        stage = 'positive and negative real routes'
        paths = {}
        for kind, stem in [('web_seed', '/api/web-seed/v1/media/'), ('torrent', '/api/torrent/v1/media/')]:
            path = stem + media_id + '/' + token
            paths[kind] = path
            probe(cp, path, kind, 404)  # Valid grant passes authorization; absent media.
            probe(cp, path, kind, 404, method='HEAD')
            if kind == 'web_seed':
                probe(cp, path + '/' + media_id, kind, 404)
            probe(cp, stem + media_id + '/' + marker, kind, 403)
            probe(cp, path + '?sentinel=' + marker + '&grant=' + token, kind, 404)
            probe(cp, stem + 'malformed/' + token + '/extra/' + marker, kind, 404)
            encoded = ''.join('%%%02X' % b for b in token.encode())
            probe(cp, stem + media_id + '/' + encoded, kind, 404)
        query = urllib.parse.urlencode({'info_hash': bytes.fromhex(issued['hash'])[:20],
            'peer_id': b'-ARKNET-A-0000000001', 'port': '6881', 'uploaded': '0',
            'downloaded': '0', 'left': '1', 'compact': '1'})
        tracker_path = '/announce/' + token + '?' + query
        status, body = probe(tp, tracker_path, 'tracker', 200)
        decoded = helper_module.bdecode(body)
        check(b'failure reason' not in decoded and b'peers' in decoded, 'tracker positive announce not accepted')
        probe(tp, '/announce/' + marker + '?' + query, 'tracker', 403)
        probe(tp, tracker_path + '&endpoint_token=' + marker, 'tracker', 403)
        probe(tp, tracker_path + '&endpoint_token=' + urllib.parse.quote(token, safe=''), 'tracker', 403)
        for port in (cp, tp):
            probe(port, '/unknown/' + marker + '/' + token + '?grant=' + token, 'other', 404)
            probe(port, '/%ZZ/' + token + '?sentinel=' + marker, 'other', 400)
        stage = 'plaintext refusal'
        for port in (cp, tp):
            status, response_headers, _ = request(port, '/unknown/' + token, headers=headers)
            check(status == 400, 'plaintext request not refused')
            scan_sink('plaintext-location', response_headers.get('Location', '').encode(), secret_values)
            check('Location' not in response_headers, 'plaintext request redirected')
            expected.append((labels['other'], 400))
        stage = 'Agent and UI live requests'
        agent_path = '/api/sentinel/' + marker + '/' + token + '?grant=' + token
        for port in (ap, up):
            status, _, _ = request(port, agent_path, headers=headers)
            check(status in (401, 404), 'real Agent path not rejected')
            encoded_path = '/api/sentinel/' + ''.join('%%%02X' % b for b in marker.encode()) + '/' + token
            check(request(port, encoded_path, headers=headers)[0] in (401, 404), 'encoded Agent path not rejected')
        direct = request(ap, '/api/v1/media?sentinel=' + marker, headers=headers)
        proxied = request(up, '/api/v1/media?sentinel=' + marker, headers=headers)
        check(direct[0] == proxied[0] and direct[2] == proxied[2] and bool(direct[2]),
              'UI did not forward real Agent status response')
        check(direct[0] in (200, 401), 'Agent status endpoint unavailable')
        check(request(uhp, '/', context=context)[0] == 200, 'verified TLS UI unavailable')
        stage = 'forced UI proxy error'
        node.call('stop', '-t', '5', 'arknet-agent', timeout=30)
        status, _, _ = request(up, agent_path, headers=headers)
        check(status == 502, 'stopped Agent did not force UI 502')
        stage = 'forced nginx upstream errors'
        core.call('stop', '-t', '5', 'arknet-core', 'arknet-tracker', timeout=30)
        for service, network in upstream_networks.items():
            check(not inspect_container(core, service)['State']['Running'], 'upstream did not stop')
            network_state = json.loads(run(['docker', 'network', 'inspect', network]).stdout)[0]
            check(network_state['Labels'].get('com.docker.compose.project') == core.name,
                  'refusal fixture network is not owned by this run')
            # Closed listeners, not stub HTTP servers: preserve the stopped
            # upstream's DNS name on a live network namespace. Otherwise Docker
            # may black-hole the retired IP and produce a platform-dependent
            # 504 instead of exercising nginx's required connection-refused 502.
            run(['docker', 'run', '-d', '--name', prefix + '-' + service + '-closed',
                 '--network', network, '--network-alias', service, '--entrypoint', 'sleep',
                 config['services']['arknet-ingress']['image'], '180'])
        time.sleep(11)  # Expire the shipped resolver's valid=10s positive cache.
        probe(cp, paths['web_seed'] + '?sentinel=' + marker, 'web_seed', 502)
        probe(cp, paths['torrent'] + '?sentinel=' + marker, 'torrent', 502)
        probe(tp, tracker_path, 'tracker', 502)
        stage = 'five log sinks'
        time.sleep(0.3)
        sinks = {}
        for stack, services in [(core, ('arknet-core', 'arknet-tracker', 'arknet-ingress')),
                                (node, ('arknet-agent', 'arknet-ui'))]:
            for service in services:
                data = capture_logs(stack, service, root)
                check(bool(data.strip()), 'empty component log sink: ' + service)
                scan_sink(service, data, secret_values)
                sinks[service] = data
        require_access(sinks['arknet-ingress'], expected)
        # The actual proxy-error logger must have run, not merely startup output.
        check(b'proxy' in sinks['arknet-ui'].lower(), 'missing UI proxy-error evidence')
        print('PASS: forwarding statuses and all five raw log sinks are redacted', flush=True)
        stage = 'unsafe instrumentation negative control'
        original = (ROOT / 'deploy/core/ingress/nginx.conf').read_text()
        check('$arknet_log_route' in original, 'negative control mutation target missing')
        # Mutate the real logging format, not a fake scanner-only byte string.
        unsafe = original.replace('"route":"$arknet_log_route"', '"route":"$request_uri"')
        check(unsafe != original, 'negative control mutation did not change logger')
        unsafe_file = root / 'unsafe-nginx.conf'
        unsafe_file.write_text(unsafe)
        mutant_file = private_override(root, 'unsafe-overlay', {'arknet-ingress': {'volumes': [
            {'type': 'bind', 'source': str(unsafe_file), 'target': '/etc/nginx/nginx.conf', 'read_only': True}]}})
        mutant = Stack(core.name, root, core_environment, core.files + [mutant_file])
        mutant.call('up', '-d', '--no-deps', '--force-recreate', 'arknet-ingress', timeout=60)
        wait_for(lambda: request(cp, paths['web_seed'], context=context)[0] == 502, 'mutated ingress')
        time.sleep(0.2)
        leaked = capture_logs(mutant, 'arknet-ingress', root, '-negative-control')
        check(bool(leaked.strip()), 'negative control produced no logs')
        try:
            scan_sink('arknet-ingress', leaked, secret_values)
        except CheckFailure as error:
            check(str(error) == 'sensitive data in sink: arknet-ingress', 'negative control failed for unrelated reason')
        else:
            raise CheckFailure('unsafe instrumentation was not detected')
        print('PASS: five real sinks, verified TLS, forwarding, forced errors, negative control', flush=True)
    except CheckFailure as error:
        print('FAIL [' + stage + ']: ' + str(error), flush=True)
        return_code = 1
    except Exception:
        # Never expose arbitrary exception text: urllib/subprocess often embed URLs.
        print('FAIL [' + stage + ']: unexpected private harness error', flush=True)
        return_code = 1
    else:
        return_code = 0
    finally:
        signal.alarm(0)
        signal.signal(signal.SIGALRM, previous_alarm)
        for suffix in ('-tls-owner', '-signing-owner', '-arknet-core-closed', '-arknet-tracker-closed'):
            try:
                run(['docker', 'rm', '-f', prefix + suffix], check=False, timeout=15)
                check(run(['docker', 'inspect', prefix + suffix], check=False, timeout=15).returncode != 0,
                      'fixture helper cleanup incomplete')
            except CheckFailure:
                cleanup_ok = False
        for stack in reversed(stacks):
            try:
                stack.call('down', '--volumes', '--remove-orphans', '--timeout', '5', timeout=60)
                for kind in ('container', 'network', 'volume'):
                    noun = {'container': 'ps', 'network': 'network', 'volume': 'volume'}[kind]
                    command = (['docker', 'ps', '-aq'] if kind == 'container' else ['docker', noun, 'ls', '-q'])
                    output = run(command + ['--filter', 'label=com.docker.compose.project=' + stack.name], timeout=15).stdout
                    check(not output.strip(), 'disposable project cleanup incomplete')
            except CheckFailure:
                cleanup_ok = False
        try:
            run(['docker', 'volume', 'rm', prefix + '-signing'], check=False, timeout=15)
            check(run(['docker', 'volume', 'inspect', prefix + '-signing'], check=False, timeout=15).returncode != 0,
                  'signing fixture cleanup incomplete')
        except CheckFailure:
            cleanup_ok = False
        for image in images:
            try:
                run(['docker', 'image', 'rm', image], check=False, timeout=15)
                check(run(['docker', 'image', 'inspect', image], check=False, timeout=15).returncode != 0,
                      'disposable image cleanup incomplete')
            except CheckFailure:
                cleanup_ok = False
        shutil.rmtree(root)
        print(('PASS' if cleanup_ok else 'FAIL') + ': exact disposable project cleanup', flush=True)
    return return_code if cleanup_ok else 1


if __name__ == '__main__':
    raise SystemExit(main())
