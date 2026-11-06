"""Scoped NodeNet deployment helpers. Never print secret payloads/config."""
from pathlib import Path
import base64
import json
import os
import shlex
import subprocess

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
INVENTORY = json.loads((HERE / 'inventory.json').read_text())
MACHINES = {m['hostname']: m for m in INVENTORY['machines']}


def ssh(machine, command, data=None, timeout=600):
    if isinstance(machine, str):
        machine = MACHINES[machine]
    args = ['ssh', '-i', str(Path.home() / '.ssh/arknet-live-test'),
            '-o', 'IdentitiesOnly=yes', '-o', 'BatchMode=yes',
            '-o', 'StrictHostKeyChecking=yes',
            '-o', 'UserKnownHostsFile=' + str(Path.home() / '.ssh/arknet-live-test-known_hosts'),
            '-o', 'HostKeyAlias=' + machine['lan_ip'],
            'arknet@' + machine['tailscale_ip'], command]
    result = subprocess.run(args, input=data, capture_output=True, timeout=timeout)
    if result.returncode:
        # Callers can explicitly allowlist non-secret diagnostics when needed.
        raise RuntimeError(machine['hostname'] + ': remote command failed (exit ' + str(result.returncode) + '); output withheld')
    return result.stdout


CORE_COMPOSE = 'cd /opt/arknet && sudo docker compose --env-file runtime.env -f compose.json'
ADMIN_PERMISSIONS = {'nodes.decide', 'users.admin', 'media.admin', 'relays.read'}


def core_machine():
    return next(m for m in MACHINES.values() if m['role'] == 'core')


def mint_core_admin(name, *permissions, ttl='2h'):
    """Mint a scoped Core admin token on the Core VM's shell; return (token, token_id).

    The token is held only in memory by the caller and is never printed.
    """
    if not permissions or not set(permissions) <= ADMIN_PERMISSIONS or len(set(permissions)) != len(permissions):
        raise ValueError('name one or more known permissions, without repeats')
    args = ['create-token', '--name', name, '--ttl', ttl]
    for permission in permissions:
        args += ['--permission', permission]
    out = ssh(core_machine(), CORE_COMPOSE + ' exec -T arknet-core arknet-core admin ' + ' '.join(shlex.quote(a) for a in args))
    result = json.loads(out)
    if not result.get('token', '').startswith('arknet_adm_') or sorted(result.get('permissions', [])) != sorted(permissions):
        raise RuntimeError('Core did not mint the requested scoped token')
    return result['token'], result['token_id']


def revoke_core_admin(token_id):
    ssh(core_machine(), CORE_COMPOSE + ' exec -T arknet-core arknet-core admin revoke-token --token-id ' + shlex.quote(token_id))


def put(machine, path, content, mode=0o644):
    if isinstance(content, str):
        content = content.encode()
    payload = json.dumps({'path': path, 'content': base64.b64encode(content).decode(), 'mode': mode}).encode()
    code = ('import json,sys,base64,os; from pathlib import Path; '
            'd=json.load(sys.stdin); p=Path(d["path"]); p.parent.mkdir(parents=True,exist_ok=True); '
            'fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,d["mode"]); '
            'os.write(fd,base64.b64decode(d["content"])); os.close(fd); p.chmod(d["mode"])')
    ssh(machine, 'sudo python3 -c ' + shlex.quote(code), payload)


def load_secrets():
    import secrets
    directory = HERE / 'secrets'
    directory.mkdir(mode=0o700, exist_ok=True)
    directory.chmod(0o700)
    path = directory / 'runtime.json'
    if path.exists():
        return json.loads(path.read_text())
    # No shared Core admin token is stored (ARKSEC-005): scripts mint scoped,
    # short-lived tokens with mint_core_admin() and revoke them when done.
    data = {'machines': {}, 'global_users': {}}
    for host in MACHINES:
        data['machines'][host] = {'database_password': secrets.token_hex(32),
                                  'transfer_token': secrets.token_hex(32),
                                  'operator_password': secrets.token_urlsafe(24),
                                  'jellyfin_password': secrets.token_urlsafe(24)}
    for name in ['user-a', 'user-b', 'user-c']:
        data['global_users'][name] = {'username': name, 'password': secrets.token_urlsafe(24)}
    save_secrets(data)
    return data


def save_secrets(data):
    path = HERE / 'secrets/runtime.json'
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w') as stream:
        json.dump(data, stream, indent=2)
        stream.write('\n')
    path.chmod(0o600)
