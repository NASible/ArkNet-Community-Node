#!/usr/bin/env python3
"""Render pilot-only Compose and start exactly one explicitly named VM stack."""
import argparse
import datetime
import hashlib
import json
import shlex
from ops import HERE, ROOT, MACHINES, load_secrets, put, ssh

parser = argparse.ArgumentParser()
parser.add_argument('host', choices=list(MACHINES))
parser.add_argument('--configure-only', action='store_true')
args = parser.parse_args()
machine = MACHINES[args.host]
secrets = load_secrets()
local = secrets['machines'][args.host]
record = json.loads((HERE / 'build-record.json').read_text())
images = {key: value['tag'] for key, value in record['images'].items()}
postgres_image = 'postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5'
jellyfin_image = 'jellyfin/jellyfin:12.2@sha256:357724bf0ae27a672c7cbaa899db2d9abeb13dbd8657ccce750258a4c059d037'
project = args.host
core = machine['role'] == 'core'
base = '/opt/arknet'
db = 'arknet_core' if core else 'arknet_node'
# Community Nodes never receive Core administrative credentials (security review
# requirement); each machine gets only the secrets its own services use.
env = {'POSTGRES_PASSWORD': local['database_password']}
if machine['role'] == 'core':
    env['CORE_ADMIN_TOKEN'] = secrets['core_admin_token']
else:
    env.update({'TRANSFER_TOKEN': local['transfer_token'], 'JELLYFIN_API_KEY': local.get('jellyfin_api_key', '')})
put(machine, base + '/runtime.env', '\n'.join(k + '=' + v for k, v in env.items()) + '\n', 0o600)

hardening = {'restart': 'unless-stopped', 'cap_drop': ['ALL'], 'security_opt': ['no-new-privileges:true'],
             'logging': {'driver': 'json-file', 'options': {'max-size': '5m', 'max-file': '2'}}}
ca_mount = '/etc/ssl/certs/ca-certificates.crt:/etc/ssl/certs/ca-certificates.crt:ro'
extra_hosts = {'core.nodenet.arknet.media': '100.64.0.21', 'tracker.nodenet.arknet.media': '100.64.0.21'}
pgenv = {'PGHOST': 'postgres', 'PGPORT': '5432', 'PGSSLMODE': 'disable',
         'PGDATABASE': db, 'PGUSER': 'arknet', 'PGPASSWORD': '${POSTGRES_PASSWORD}'}

def probe(command):
    return {'test': command, 'interval': '5s', 'timeout': '3s', 'retries': 30, 'start_period': '10s'}

services = {'postgres': {
    'image': postgres_image, 'restart': 'unless-stopped', 'mem_limit': '384m',
    'command': ['postgres', '-c', 'shared_buffers=64MB', '-c', 'max_connections=40'],
    'environment': {'POSTGRES_DB': db, 'POSTGRES_USER': 'arknet', 'POSTGRES_PASSWORD': '${POSTGRES_PASSWORD}'},
    'volumes': ['postgres-data:/var/lib/postgresql'], 'networks': ['database'],
    'healthcheck': probe(['CMD-SHELL', 'pg_isready -U arknet -d ' + db]),
    'logging': hardening['logging'],
}}
networks = {'database': {'internal': True}, 'egress': {}}
volumes = {'postgres-data': {}}
if core:
    ssh(machine, 'sudo install -d -m 0700 /etc/arknet/secrets && sudo install -d -o 10001 -g 10001 -m 0755 /srv/arknet/web-seed')
    # Ed25519 PKCS8 DER contains the 32-byte seed; SPKI DER the 32-byte public key.
    generation = '''import os,subprocess,base64
from pathlib import Path
os.umask(0o077)
a=Path('/etc/arknet/secrets/tracker-signing-key'); b=Path('/etc/arknet/secrets/tracker-verify-key')
if a.exists()!=b.exists(): raise RuntimeError('Partial tracker keypair; refusing overwrite')
if not a.exists():
 private=subprocess.check_output(['openssl','genpkey','-algorithm','ED25519','-outform','DER'])
 public=subprocess.check_output(['openssl','pkey','-inform','DER','-pubout','-outform','DER'],input=private)
 assert len(private)==48 and len(public)==44
 a.write_bytes(base64.b64encode(private[-32:]+public[-32:])); b.write_bytes(base64.b64encode(public[-32:]))
 a.chmod(0o400); os.chown(a,10001,10001); b.chmod(0o444)
print('TRACKER_KEYS_READY')
'''
    print(ssh(machine, 'sudo python3 -c ' + shlex.quote(generation)).decode().strip())
    services['arknet-core'] = {**hardening, 'image': images['core'], 'mem_limit': '256m',
        'environment': {**pgenv, 'ARKNET_CORE_ADDR': ':8080', 'ARKNET_CORE_DEV_ADMIN_ENABLED': 'true',
            'ARKNET_CORE_DEV_ADMIN_TOKEN': '${CORE_ADMIN_TOKEN}', 'ARKNET_TRACKER_ANNOUNCE_URL': 'https://tracker.nodenet.arknet.media',
            'ARKNET_TRACKER_SIGNING_KEY_FILE': '/run/secrets/tracker-signing-key',
            'ARKNET_CORE_WEB_SEED_ORIGIN': 'https://core.nodenet.arknet.media', 'ARKNET_CORE_WEB_SEED_ROOT': '/srv/arknet/web-seed'},
        'ports': ['127.0.0.1:8083:8080'], 'networks': ['database', 'egress'],
        'volumes': ['/etc/arknet/secrets/tracker-signing-key:/run/secrets/tracker-signing-key:ro', '/srv/arknet/web-seed:/srv/arknet/web-seed:ro'],
        'depends_on': {'postgres': {'condition': 'service_healthy'}},
        'healthcheck': probe(['CMD', 'wget', '-q', '-O', '/dev/null', 'http://127.0.0.1:8080/health'])}
    services['arknet-tracker'] = {**hardening, 'image': images['tracker'], 'mem_limit': '128m',
        'environment': {'ARKNET_TRACKER_BIND_ADDR': '0.0.0.0:6969', 'ARKNET_TRACKER_VERIFY_KEY_FILE': '/run/secrets/tracker-verify-key',
                        'ARKNET_TRACKER_ANNOUNCE_INTERVAL': '30s', 'ARKNET_TRACKER_PEER_TTL': '90s'},
        'volumes': ['/etc/arknet/secrets/tracker-verify-key:/run/secrets/tracker-verify-key:ro'],
        'ports': ['127.0.0.1:6969:6969'], 'networks': ['egress'], 'read_only': True,
        'healthcheck': probe(['CMD', 'wget', '-q', '-O', '/dev/null', 'http://127.0.0.1:6969/health'])}
else:
    networks.update({'control': {'internal': True}, 'media-services': {'internal': True}, 'ui-edge': {}})
    put(machine, base + '/ui-nginx.conf', (HERE / 'ui-nginx.conf').read_bytes())
    volumes.update({'appdata': {}, 'jellyfin-cache': {}, 'transfers': {}, 'transfer-state': {}})
    ssh(machine, 'findmnt -rn /srv/arknet/media >/dev/null && sudo chown 10001:10001 /srv/arknet/media && sudo chmod 0755 /srv/arknet/media')
    services['jellyfin'] = {'image': jellyfin_image, 'restart': 'unless-stopped', 'mem_limit': '768m',
        'environment': {'TZ': 'America/Anchorage', 'JELLYFIN_PublishedServerUrl': 'https://jellyfin-' + args.host[-1] + '.nodenet.arknet.media'},
        'volumes': ['appdata:/config', 'jellyfin-cache:/cache', '/srv/arknet/media:/srv/arknet/media:ro'],
        'ports': ['127.0.0.1:8096:8096'], 'networks': ['media-services', 'egress'],
        'healthcheck': {'test': ['CMD-SHELL', 'curl --noproxy localhost -fsS http://127.0.0.1:8096/health'], 'interval': '10s', 'timeout': '5s', 'retries': 30, 'start_period': '30s'},
        'logging': hardening['logging']}
    services['arknet-transfer'] = {**hardening, 'image': images['transfer'], 'mem_limit': '256m',
        'environment': {'ARKNET_TRANSFER_ADDR': '0.0.0.0:8084', 'ARKNET_TRANSFER_TOKEN': '${TRANSFER_TOKEN}',
            'ARKNET_TRANSFER_TRACKER_ORIGIN': 'https://tracker.nodenet.arknet.media',
            'ARKNET_TRANSFER_WEB_SEED_ORIGIN': 'https://core.nodenet.arknet.media',
            'ARKNET_TRANSFER_STAGING_DIR': '/srv/arknet/transfers', 'ARKNET_TRANSFER_STATE_DIR': '/srv/arknet/transfer-state',
            'ARKNET_TRANSFER_MEDIA_DIR': '/srv/arknet/media', 'ARKNET_TRANSFER_WORKER': '/usr/local/lib/arknet/worker.py',
            'ARKNET_TRANSFER_LISTEN_INTERFACES': '0.0.0.0:6881', 'SSL_CERT_FILE': '/etc/ssl/certs/ca-certificates.crt'},
        'extra_hosts': extra_hosts, 'volumes': ['transfers:/srv/arknet/transfers', 'transfer-state:/srv/arknet/transfer-state', '/srv/arknet/media:/srv/arknet/media', ca_mount],
        'networks': ['control', 'egress'], 'read_only': True, 'tmpfs': ['/tmp:size=16m,mode=1777'],
        'healthcheck': probe(['CMD', 'python3', '-c', "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8084/health',timeout=2).read()"]) }
    services['arknet-agent'] = {**hardening, 'image': images['agent'], 'mem_limit': '128m',
        'environment': {**pgenv, 'ARKNET_AGENT_ADDR': ':8081', 'ARKNET_CORE_URL': 'https://core.nodenet.arknet.media',
            'ARKNET_HEARTBEAT_INTERVAL': '30s', 'ARKNET_MEDIA_STORAGE_ALLOCATION_GIB': '20',
            'ARKNET_TRANSFER_URL': 'http://arknet-transfer:8084', 'ARKNET_TRANSFER_TOKEN': '${TRANSFER_TOKEN}',
            'ARKNET_TRANSFER_STAGING_DIR': '/srv/arknet/transfers', 'ARKNET_MEDIA_DIR': '/srv/arknet/media',
            'ARKNET_TRANSFER_RECONCILE_INTERVAL': '30s', 'ARKNET_TRANSFER_TRACKER_ORIGIN': 'https://tracker.nodenet.arknet.media',
            'JELLYFIN_API_URL': 'http://jellyfin:8096', 'JELLYFIN_API_KEY': '${JELLYFIN_API_KEY}',
            'ARKNET_RELEASE_MANIFEST': '/etc/arknet/release.json', 'SSL_CERT_FILE': '/etc/ssl/certs/ca-certificates.crt'},
        'extra_hosts': extra_hosts, 'volumes': ['transfers:/srv/arknet/transfers', '/srv/arknet/media:/srv/arknet/media', '/opt/arknet/release.json:/etc/arknet/release.json:ro', ca_mount],
        'ports': ['127.0.0.1:8082:8081'], 'networks': ['database', 'control', 'media-services', 'egress'],
        'depends_on': {'postgres': {'condition': 'service_healthy'}, 'arknet-transfer': {'condition': 'service_healthy'}},
        'healthcheck': probe(['CMD', 'wget', '-q', '-O', '/dev/null', 'http://127.0.0.1:8081/health'])}
    services['arknet-ui'] = {'image': images['ui'], 'restart': 'unless-stopped', 'mem_limit': '64m',
        'ports': ['127.0.0.1:5174:5173'], 'networks': ['control', 'ui-edge'],
        'volumes': ['/opt/arknet/ui-nginx.conf:/etc/nginx/conf.d/default.conf:ro'],
        'depends_on': {'arknet-agent': {'condition': 'service_healthy'}},
        'healthcheck': probe(['CMD', 'wget', '-q', '-O', '/dev/null', 'http://127.0.0.1:5173/']), 'logging': hardening['logging']}

compose = {'name': project, 'services': services, 'networks': networks, 'volumes': volumes}
encoded = json.dumps(compose, indent=2) + '\n'
put(machine, base + '/compose.json', encoded)
# JSON is a YAML subset; keep a byte-identical .yaml entry for the strict
# release manifest's allowed Compose filenames.
put(machine, base + '/compose.yaml', encoded)
if not core:
    manifest = json.loads((ROOT / 'deploy/node/release.json').read_text())
    manifest['compose'] = {'project': project, 'files': [{'path': 'compose.yaml', 'sha256': hashlib.sha256(encoded.encode()).hexdigest()}]}
    manifest['components']['postgresql']['image'] = postgres_image
    for component, key in [('arknet-agent', 'agent'), ('arknet-ui', 'ui'), ('arknet-transfer', 'transfer')]:
        manifest['components'][component]['image'] = images[key]
    put(machine, base + '/release.json', json.dumps(manifest, indent=2) + '\n')
# Validate quietly: resolved config includes secrets and must never be printed.
command = 'cd /opt/arknet && sudo docker compose --env-file runtime.env -f compose.json'
ssh(machine, command + ' config --quiet')
print('COMPOSE_VALIDATED', args.host, flush=True)
if not args.configure_only:
    print(ssh(machine, command + ' up -d --wait --wait-timeout 240', timeout=360).decode())
    status = ssh(machine, command + ' ps --format json').decode()
    # Compose ps uses JSON-lines on this installed version; environment is absent.
    print('STACK_STATUS', args.host, status, flush=True)
