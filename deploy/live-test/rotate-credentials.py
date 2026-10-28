#!/usr/bin/env python3
"""Rotate every live-test credential in place, then prove new accepted / old rejected.

Rotates: the three global-user passwords, the three Jellyfin administrator
passwords (and their access tokens), the Core admin token, every PostgreSQL
role password, and every Agent-to-transfer token. Removes the unused
operator_password entries and the Core admin token from Node VMs.

Crash safety: old and new values are written to secrets/rotation-pending.json
(0600) before anything changes. Every step is idempotent, so rerunning after a
failure resumes from the pending file. The pending file is deleted only after
every check passes.

Never prints a secret: secrets travel to VMs only on stdin, never in argv, and
API errors are reported by status code only. The last line is "EXIT <code>".
"""
from __future__ import annotations

import json
import os
import secrets as rng
import shlex
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

from ops import HERE, MACHINES, load_secrets, save_secrets, ssh
from private_api import TLS, request, wait

PENDING = HERE / 'secrets' / 'rotation-pending.json'
ACCESS = HERE / 'secrets' / 'access.txt'
CORE = 'https://core.nodenet.arknet.media'
CORE_HOST = next(h for h, m in MACHINES.items() if m['role'] == 'core')
NODES = [h for h, m in MACHINES.items() if m['role'] == 'node']
JELLYFIN_CLIENT = ('MediaBrowser Client="NodeNet operator", Device="operator-mac", '
                   'DeviceId="nodenet-private-pilot-{}", Version="0.0.1-dev"')
checks = []


def step(message):
    print('STEP', message, flush=True)


def passed(message):
    checks.append(message)
    print('PASS', message, flush=True)


def write_private(path, text):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w') as stream:
        stream.write(text)
    path.chmod(0o600)


# ------------------------------------------------------------------ plan
def make_plan(state):
    old = {'core_admin_token': state['core_admin_token'], 'machines': {}, 'global_users': {}}
    new = {'core_admin_token': rng.token_hex(32), 'machines': {}, 'global_users': {}}
    for host in MACHINES:
        local = state['machines'][host]
        keys = ['database_password'] + (['transfer_token', 'jellyfin_password', 'jellyfin_access_token']
                                         if host in NODES else [])
        old['machines'][host] = {k: local[k] for k in keys}
        new['machines'][host] = {'database_password': rng.token_hex(32)}
        if host in NODES:
            new['machines'][host].update(transfer_token=rng.token_hex(32), jellyfin_password=rng.token_urlsafe(24))
    for name, user in state['global_users'].items():
        old['global_users'][name] = user['password']
        new['global_users'][name] = rng.token_urlsafe(24)
    return {'old': old, 'new': new}


def load_plan(state):
    if PENDING.exists():
        print('RESUME rotation from pending file', flush=True)
        return json.loads(PENDING.read_text())
    plan = make_plan(state)
    write_private(PENDING, json.dumps(plan, indent=2) + '\n')
    return plan


# ------------------------------------------------------------ helpers
def database_name(host):
    return 'arknet_core' if host == CORE_HOST else 'arknet_node'


def alter_role(host, password):
    # token_hex values contain only [0-9a-f], so no SQL quoting issues. Sent on stdin.
    sql = "ALTER ROLE arknet PASSWORD '" + password + "';\n"
    ssh(host, 'sudo docker exec -i ' + host + '-postgres-1 psql -q -U arknet -d ' + database_name(host) +
        ' -v ON_ERROR_STOP=1', sql.encode())


def database_accepts(host, password):
    """Password auth over TCP from a throwaway container on the database network (not the local trusted socket)."""
    image = ssh(host, 'sudo docker inspect ' + host + '-postgres-1 --format "{{.Config.Image}}"').decode().strip()
    inner = ('read -r PGPASSWORD; export PGPASSWORD; docker run --rm --network ' + host + '_database -e PGPASSWORD '
             + shlex.quote(image) + ' psql "host=postgres user=arknet dbname=' + database_name(host) +
             ' sslmode=disable connect_timeout=5" -tAc "SELECT 1" 2>/dev/null || echo REFUSED')
    return ssh(host, 'sudo sh -c ' + shlex.quote(inner), (password + '\n').encode()).decode().strip() == '1'


TRANSFER_PROBE = ('import sys,urllib.request,urllib.error\n'
                  't=sys.stdin.readline().strip()\n'
                  "r=urllib.request.Request('http://127.0.0.1:8084/api/transfer/v1/seeds',headers={'Authorization':'Bearer '+t})\n"
                  'try: print(urllib.request.urlopen(r,timeout=5).status)\n'
                  'except urllib.error.HTTPError as e: print(e.code)\n')


def transfer_status(host, token):
    command = 'sudo docker exec -i ' + host + '-arknet-transfer-1 python3 -c ' + shlex.quote(TRANSFER_PROBE)
    return int(ssh(host, command, (token + '\n').encode()).decode().strip())


def deploy(host):
    result = subprocess.run([sys.executable, str(HERE / 'deploy-stack.py'), host], capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError(host + ': deploy-stack failed (exit ' + str(result.returncode) + '); output withheld')


def core_login(username, password):
    return request(CORE + '/api/identity/v1/sessions', 'POST', {'username': username, 'password': password},
                   expected=(201, 401))


def node_url(host):
    return 'https://node-' + host[-1] + '.nodenet.arknet.media'


def jellyfin_url(host):
    return 'https://jellyfin-' + host[-1] + '.nodenet.arknet.media'


def node_login(host, member_id, password):
    return request(node_url(host) + '/api/v1/global-users/sessions', 'POST',
                   {'member_id': member_id, 'password': password}, expected=(201, 401))


def status_of(url, method='GET', body=None, headers=None):
    """Status code only; for endpoints (Jellyfin) whose error bodies are not JSON."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={'Content-Type': 'application/json', **(headers or {})})
    try:
        with urllib.request.urlopen(req, context=TLS, timeout=30) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, b''


def jellyfin_login(host, username, password):
    status, raw = status_of(jellyfin_url(host) + '/Users/AuthenticateByName', 'POST',
                            {'Username': username, 'Pw': password}, {'Authorization': JELLYFIN_CLIENT.format(host[-1])})
    if status not in (200, 401):
        raise RuntimeError(host + ': Jellyfin login returned HTTP ' + str(status))
    return status, json.loads(raw) if status == 200 else None


def jellyfin_auth(token):
    return {'Authorization': 'MediaBrowser Token="' + token + '"'}


def node_healthy(host):
    status = request(node_url(host) + '/api/v1/status')[1]
    ready = status.get('state') == 'CONNECTED' and status.get('jellyfin_healthy') and status.get('database_healthy')
    return status if ready else None


# ---------------------------------------------------------------- phases
def rotate_runtime_and_databases(state, plan):
    new = plan['new']
    state['core_admin_token'] = new['core_admin_token']
    for host in MACHINES:
        local = state['machines'][host]
        local.pop('operator_password', None)  # generated at bootstrap, never used by any service
        local['database_password'] = new['machines'][host]['database_password']
        if host in NODES:
            local['transfer_token'] = new['machines'][host]['transfer_token']
    save_secrets(state)
    # Core first: global-user rotation needs Core up; Nodes reconnect to it afterwards.
    for host in [CORE_HOST] + NODES:
        step(host + ': database role password, runtime.env, recreate stack')
        alter_role(host, new['machines'][host]['database_password'])
        deploy(host)
        if host in NODES:
            wait(host + ' healthy after redeploy', lambda h=host: node_healthy(h), 240)
        print('DEPLOYED', host, flush=True)


def rotate_global_users(state, plan):
    old, new = plan['old']['global_users'], plan['new']['global_users']
    status, _ = request(CORE + '/api/identity/v1/password', 'POST', {}, expected=(401, 404))
    assert status == 401, 'Core lacks the password-change endpoint; deploy the current Core image first'
    for name, user in state['global_users'].items():
        step(name + ': change password at Core')
        if core_login(name, new[name])[0] != 201:
            status, session = core_login(name, old[name])
            assert status == 201, name + ': neither the old nor the new password logs in at Core'
            request(CORE + '/api/identity/v1/password', 'POST',
                    {'current_password': old[name], 'new_password': new[name]},
                    {'Authorization': 'Bearer ' + session['token']}, expected=(204,))
        status, session = core_login(name, new[name])
        assert status == 201, name + ': new password rejected at Core'
        request(CORE + '/api/identity/v1/session', 'DELETE', headers={'Authorization': 'Bearer ' + session['token']})
        assert core_login(name, old[name])[0] == 401, name + ': old password still accepted at Core'
        passed(name + ' Core: new password accepted, old rejected')
        host = user.get('node_host') or 'nodenet-node-' + name[-1]
        # An online attempt with the old password makes the Agent revoke its cached
        # verifier (Core answers "denied"), so the old password cannot work offline later.
        assert node_login(host, user['member_id'], old[name])[0] == 401, name + ': old password accepted by its Node'
        assert node_login(host, user['member_id'], new[name])[0] == 201, name + ': new password rejected by its Node'
        passed(name + ' ' + host + ': new password accepted, old rejected (cached verifier revoked)')
        user['password'] = new[name]
        save_secrets(state)


def rotate_jellyfin(state, plan):
    for host in NODES:
        local = state['machines'][host]
        username = local['jellyfin_username']
        old_password = plan['old']['machines'][host]['jellyfin_password']
        new_password = plan['new']['machines'][host]['jellyfin_password']
        step(host + ': Jellyfin administrator password and sessions')
        status, login = jellyfin_login(host, username, new_password)
        if status != 200:
            status, login = jellyfin_login(host, username, old_password)
            assert status == 200, host + ': neither the old nor the new Jellyfin password works'
            query = urllib.parse.urlencode({'userId': local['jellyfin_user_id']})
            request(jellyfin_url(host) + '/Users/Password?' + query, 'POST',
                    {'CurrentPw': old_password, 'NewPw': new_password},
                    jellyfin_auth(login['AccessToken']), expected=(204,))
            status, login = jellyfin_login(host, username, new_password)
            assert status == 200, host + ': new Jellyfin password rejected'
        token = login['AccessToken']
        # Revoke every other device session (old tokens, browsers). API keys are not
        # devices, so the Agent's Jellyfin API key keeps working.
        _, devices = request(jellyfin_url(host) + '/Devices', headers=jellyfin_auth(token))
        current = 'nodenet-private-pilot-' + host[-1]
        for device in devices.get('Items', []):
            if device.get('Id') != current:
                request(jellyfin_url(host) + '/Devices?' + urllib.parse.urlencode({'id': device['Id']}), 'DELETE',
                        headers=jellyfin_auth(token), expected=(204,))
        assert jellyfin_login(host, username, old_password)[0] == 401, host + ': old Jellyfin password still accepted'
        old_token = plan['old']['machines'][host]['jellyfin_access_token']
        if old_token != token:
            status, _ = status_of(jellyfin_url(host) + '/Users/Me', headers=jellyfin_auth(old_token))
            assert status == 401, host + ': old Jellyfin access token still accepted'
        local['jellyfin_password'], local['jellyfin_access_token'] = new_password, token
        save_secrets(state)
        passed(host + ' Jellyfin: new password accepted, old password and old access token rejected')


def verify_machine_secrets(plan):
    old, new = plan['old'], plan['new']
    admin = CORE + '/api/admin/v1/nodes/pending'
    for token, want, label in ((new['core_admin_token'], 200, 'new'), (old['core_admin_token'], 401, 'old')):
        status, _ = request(admin, headers={'Authorization': 'Bearer ' + token}, expected=(200, 401))
        assert status == want, label + ' Core admin token: HTTP ' + str(status)
    passed('Core admin token: new accepted, old rejected')
    for host in NODES:
        count = ssh(host, 'sudo sh -c "grep -c CORE_ADMIN_TOKEN /opt/arknet/runtime.env || true"').decode().strip()
        assert count == '0', host + ': Core admin token still present on a Node'
    passed('Core admin token absent from all Node VMs')
    for host in MACHINES:
        assert database_accepts(host, new['machines'][host]['database_password']), host + ': new DB password refused'
        assert not database_accepts(host, old['machines'][host]['database_password']), host + ': old DB password accepted'
        passed(host + ' PostgreSQL: new password accepted over TCP, old refused')
    for host in NODES:
        new_status = transfer_status(host, new['machines'][host]['transfer_token'])
        old_status = transfer_status(host, old['machines'][host]['transfer_token'])
        assert new_status == 200, host + ': new transfer token got HTTP ' + str(new_status)
        assert old_status == 401, host + ': old transfer token got HTTP ' + str(old_status)
        passed(host + ' transfer API: new token accepted, old rejected')
    for host in NODES:
        wait(host + ' CONNECTED', lambda h=host: node_healthy(h), 180)
    passed('all Nodes CONNECTED with healthy database and Jellyfin')


def write_access(state):
    lines = ['NodeNet private live-test access', 'Keep this file private. Never paste it into chat or source control.',
             'Management access is restricted to this Mac over the VPN.',
             'Simulated ownership is a pilot convention, not hostile multi-operator RBAC.', '']
    for host in NODES:
        label = host[-1]
        user = state['global_users']['user-' + label]
        local = state['machines'][host]
        lines += ['USER ' + label.upper(), 'Node management: ' + node_url(host),
                  'Global username: ' + user['username'], 'Global password: ' + user['password'],
                  'Pairwise member ID for this Node: ' + user['member_id'],
                  'Jellyfin: ' + jellyfin_url(host),
                  'Jellyfin administrator username: ' + local['jellyfin_username'],
                  'Jellyfin administrator password: ' + local['jellyfin_password'], '']
    lines += ['CORE ADMINISTRATION (trusted operator)', 'Core API: ' + CORE,
              'Admin Authorization bearer token: ' + state['core_admin_token'],
              'Core has no separate administration dashboard in the current product.',
              'Global users change their own password with POST /api/identity/v1/password and a Core session.',
              'Peer Serving is OFF. Transfers use the Core web seed; use the explicit Start action.', '']
    write_private(ACCESS, '\n'.join(lines))


def main():
    state = load_secrets()
    plan = load_plan(state)
    rotate_runtime_and_databases(state, plan)
    rotate_global_users(state, plan)
    rotate_jellyfin(state, plan)
    verify_machine_secrets(plan)
    write_access(state)
    PENDING.unlink()
    print('ROTATION_VERIFIED', len(checks), 'checks passed; runtime.json and access.txt updated', flush=True)


if __name__ == '__main__':
    code = 0
    try:
        main()
    except Exception as error:  # report the type and our own message only; never response bodies
        code = 1
        message = str(error) if isinstance(error, (AssertionError, RuntimeError)) else ''
        print('FAIL', type(error).__name__ + (': ' + message if message else ''), flush=True)
    print('EXIT', code, flush=True)
    sys.exit(code)
