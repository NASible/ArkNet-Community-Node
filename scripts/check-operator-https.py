#!/usr/bin/env python3
"""Real Chromium -> UI HTTPS -> Vite -> Agent -> Postgres acceptance.

Requires Docker Compose, openssl, Node and Playwright with Chromium installed.
Set PLAYWRIGHT_MODULE to the installed Playwright module directory, and pass
--lan-address if automatic IPv4 discovery is unsuitable. Uses unique project,
ports and images; removes its stack, volumes, images and secret files even on
failure. Only sanitized assertions are logged (no browser trace or screenshot).
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from scripts import harness_scratch, operator_login  # noqa: E402

ROOT = Path(__file__).resolve().parents[1]
BASE = ROOT / 'deploy/node/compose.yaml'


def validate_browser_state(state):
    cookie = state['cookie']
    assert cookie['secure'] is True, 'session cookie must be Secure'
    assert cookie['httpOnly'] is True, 'session cookie must be HttpOnly'
    assert cookie['sameSite'] == 'Strict', 'session cookie must be SameSite Strict'
    assert cookie['path'] == '/', 'host cookie must cover root'
    assert state['local'] == 0, 'localStorage must be empty'
    assert state['session'] == 0, 'sessionStorage must be empty'
    assert state['visibleCookie'] is False, 'session cookie must be invisible to JavaScript'


def command(args, **kwargs):
    result = subprocess.run(args, capture_output=True, text=True, **kwargs)
    if result.returncode:
        # Never expose command output: setup CLI, browser and compose can hold secrets.
        raise RuntimeError('command failed: ' + Path(str(args[0])).name +
                           (' at ' + result.stderr.strip() if result.stderr.strip().startswith('BROWSER_STAGE ') else ''))
    return result.stdout


def request(url, method='GET', body=None, headers=None):
    req = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(),
                                 method=method, headers={'Content-Type': 'application/json', **(headers or {})})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                        urllib.request.HTTPSHandler(context=ssl._create_unverified_context()))
    try:
        with opener.open(req, timeout=10) as response:
            return response.status, dict(response.headers), response.read()
    except urllib.error.HTTPError as error:
        return error.code, dict(error.headers), error.read()


def ports():
    sockets = []
    try:
        for _ in range(4):
            sock = socket.socket()
            sock.bind(('0.0.0.0', 0))
            sockets.append(sock)
        return [sock.getsockname()[1] for sock in sockets]
    finally:
        for sock in sockets:
            sock.close()


def lan_address():
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
        sock.connect(('192.0.2.1', 9))
        return sock.getsockname()[0]


BROWSER = r'''
const fs = require('node:fs');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE);
const config = JSON.parse(fs.readFileSync(0, 'utf8'));
let stage = 'launch';
(async () => {
  const browser = await chromium.launch({headless:true});
  try {
    const context = await browser.newContext({ignoreHTTPSErrors:true});
    const page = await context.newPage();
    page.setDefaultTimeout(20000);
    const check = (ok, message) => {if (!ok) throw new Error(message);};
    stage = 'navigate';
    await page.goto(config.url);
    stage = 'signin-heading';
    await page.getByRole('heading', {name:'Operator sign-in', exact:true}).waitFor();
    stage = 'anonymous-status';
    check((await page.request.get(config.url + '/api/v1/status')).status() === 401, 'anonymous denied');
    stage = 'login';
    await page.getByLabel('Username', {exact:true}).fill(config.username);
    await page.getByLabel('Password', {exact:true}).fill(config.password);
    await page.getByLabel('Authenticator code', {exact:true}).fill(config.totp);
    const login = page.waitForResponse(r => r.url().endsWith('/api/v1/operator/sessions') && r.request().method() === 'POST');
    await page.getByRole('button', {name:'Sign in', exact:true}).click();
    check((await login).status() === 201, 'login status');
    await page.getByRole('button', {name:'Sign out', exact:true}).waitFor();
    check((await page.request.get(config.url + '/api/v1/status')).status() === 200, 'authenticated management');
    const cookie = (await context.cookies()).find(c => c.name === '__Host-arknet_operator');
    check(Boolean(cookie), 'browser accepted host cookie');
    const state = async () => page.evaluate(() => ({local:localStorage.length, session:sessionStorage.length,
      visibleCookie:document.cookie.includes('__Host-arknet_operator')}));
    const signedIn = {...await state(), cookie:cookie && {secure:cookie.secure,httpOnly:cookie.httpOnly,sameSite:cookie.sameSite,path:cookie.path}};
    await page.reload();
    await page.getByRole('button', {name:'Sign out', exact:true}).waitFor();
    const logout = page.waitForResponse(r => r.url().endsWith('/api/v1/operator/session') && r.request().method() === 'DELETE');
    await page.getByRole('button', {name:'Sign out', exact:true}).click();
    check((await logout).status() === 204, 'logout status');
    await page.getByRole('heading', {name:'Operator sign-in', exact:true}).waitFor();
    check(!(await context.cookies()).some(c => c.name === cookie.name), 'logout cleared cookie');
    check((await page.request.get(config.url + '/api/v1/status')).status() === 401, 'logout denied management');
    const replay = await context.request.get(config.url + '/api/v1/operator/session', {headers:{Cookie:cookie.name + '=' + cookie.value}});
    check(replay.status() === 401, 'logout revoked session server-side');
    const signedOut = await state();
    await page.reload();
    await page.getByRole('heading', {name:'Operator sign-in', exact:true}).waitFor();
    console.log(JSON.stringify({signedIn,signedOut}));
  } finally {await browser.close();}
})().catch(error => {
  const network = String(error.message || '').match(/net::[A-Z_]+/);
  console.error('BROWSER_STAGE ' + stage + (network ? ' ' + network[0] : ''));
  process.exitCode=1;
});
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--lan-address')
    parser.add_argument('--log-dir', type=Path)
    args = parser.parse_args()
    project = 'arknet-https-' + secrets.token_hex(5)
    # macOS system TMPDIR is not necessarily shared with Docker Desktop.
    scratch = harness_scratch.scratch_root()
    work = Path(tempfile.mkdtemp(prefix=project + '-', dir=scratch))
    logs = args.log_dir or scratch / (project + '-logs')
    logs.mkdir(parents=True, exist_ok=True)
    log = logs / 'acceptance.log'
    def report(text):
        print(text, flush=True)
        with log.open('a') as out:
            out.write(text + '\n')
    compose = ['docker', 'compose', '-p', project, '--env-file', str(work / '.env'),
               '-f', str(BASE), '-f', str(work / 'override.json')]
    images = [project + '-agent', project + '-ui']
    prepared = False
    try:
        module = os.environ.get('PLAYWRIGHT_MODULE')
        if not module:
            module = command(['node', '-p', 'require.resolve("playwright")']).strip()
        lan = args.lan_address or lan_address()
        assert not lan.startswith('127.') and lan != '0.0.0.0', 'need a real LAN address'
        agent, http, https, member = ports()
        tls = work / 'tls'
        tls.mkdir()
        command(['openssl', 'ecparam', '-name', 'prime256v1', '-genkey', '-noout',
                 '-out', str(tls / 'key.pem')])
        command(['openssl', 'req', '-new', '-x509', '-key', str(tls / 'key.pem'),
                 '-out', str(tls / 'cert.pem'), '-days', '1', '-subj', '/CN=arknet-https-acceptance'])
        (tls / 'key.pem').chmod(0o644)  # disposable certificate, readable by non-root UI
        password = secrets.token_hex(24)
        env = work / '.env'
        env.write_text(f'POSTGRES_PASSWORD={password}\nARKNET_AGENT_PORT={agent}\nARKNET_MEMBER_PORT={member}\nARKNET_UI_PORT={http}\n'
                       f'ARKNET_UI_HTTPS_BIND=0.0.0.0:{https}\nARKNET_TLS_DIR={tls}\n')
        env.chmod(0o600)
        (work / 'override.json').write_text(json.dumps({'services': {
            'arknet-agent': {'image': images[0]}, 'arknet-ui': {'image': images[1]}}}))
        prepared = True
        report('PROJECT ' + project)
        command(compose + ['build', 'arknet-agent', 'arknet-ui'], timeout=600)
        command(compose + ['up', '-d', '--wait', '--wait-timeout', '180', 'postgres', 'arknet-agent', 'arknet-ui'], timeout=240)
        report('PASS real Agent/Postgres/UI healthy')
        assert request(f'http://127.0.0.1:{member}/health')[0] == 200
        assert request(f'http://127.0.0.1:{member}/api/v1/status')[0] == 404
        assert request(f'http://127.0.0.1:{agent}/api/v1/global-users/sessions', 'POST', {})[0] == 404
        report('PASS member and management listeners are route-separated')
        assert request(f'http://127.0.0.1:{http}/')[0] == 200
        report('PASS loopback HTTP accessible')
        try:
            with socket.create_connection((lan, http), timeout=3):
                pass
        except ConnectionRefusedError:
            report('PASS LAN HTTP connection refused')
        else:
            raise AssertionError('LAN HTTP was not refused')
        url = f'https://{lan}:{https}'
        assert request(url + '/')[0] == 200
        report('PASS LAN HTTPS accessible')
        code = operator_login.setup_code(command(compose + ['exec', '-T', 'arknet-agent', '/usr/local/bin/arknet-agent', 'operator', 'setup-code']))
        operator = operator_login.bootstrap(url, code, request)
        assert request(url + '/api/v1/operator/session', 'DELETE', headers=operator.headers)[0] == 204
        # Enrollment consumed this time-step; use the next accepted TOTP step.
        config = {'url': url, 'username': operator.username, 'password': operator.password,
                  'totp': operator_login.totp(operator.totp_secret, offset=1)}
        output = command(['node', '-e', BROWSER], input=json.dumps(config),
                         env={**os.environ, 'PLAYWRIGHT_MODULE': module}, timeout=120)
        states = json.loads(output)
        validate_browser_state(states['signedIn'])
        assert states['signedOut'] == {'local': 0, 'session': 0, 'visibleCookie': False}
        (logs / 'browser-state.json').write_text(json.dumps(states, indent=2) + '\n')
        report('PASS Chromium UI password/TOTP login, reload, logout, revoked-cookie replay')
        report('PASS Secure HttpOnly SameSite=Strict host cookie; empty local/session storage')
    except Exception as error:
        report('FAIL ' + type(error).__name__ + (' ' + str(error) if isinstance(error, RuntimeError) else ' (sensitive command output suppressed)'))
        raise SystemExit(1)
    finally:
        cleanup_ok = True
        if prepared:
            try:
                command(compose + ['down', '-v', '--remove-orphans'], timeout=120)
                for image in images:
                    subprocess.run(['docker', 'image', 'rm', image], capture_output=True)
                for kind in ('container', 'volume', 'network'):
                    remaining = command(['docker', kind, 'ls', '-q', '--filter', 'label=com.docker.compose.project=' + project])
                    assert not remaining.strip()
                for image in images:
                    result = subprocess.run(['docker', 'image', 'inspect', image], capture_output=True)
                    assert result.returncode != 0
            except Exception:
                cleanup_ok = False
        shutil.rmtree(work)
        report(('PASS' if cleanup_ok else 'FAIL') + ' scoped resource cleanup')
        print('LOG ' + str(log), flush=True)
        if not cleanup_ok:
            raise SystemExit(1)


if __name__ == '__main__':
    main()
