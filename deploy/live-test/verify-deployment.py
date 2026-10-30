#!/usr/bin/env python3
"""Read-only verification of real services, image IDs, and private boundaries."""
import datetime
import json
import shlex
import socket
import subprocess
from ops import HERE, MACHINES, ssh
from private_api import request

build = json.loads((HERE / 'build-record.json').read_text())
proof = {'verified_at': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'machines': {}, 'boundaries': {}}
for host, machine in MACHINES.items():
    raw = ssh(host, 'cd /opt/arknet && sudo docker compose --env-file runtime.env -f compose.json ps --format json')
    rows = [json.loads(line) for line in raw.decode().splitlines() if line.strip()]
    expected_count = 3 if machine['role'] == 'core' else 5
    assert len(rows) == expected_count and all(row['State'] == 'running' and row.get('Health') == 'healthy' for row in rows)
    images = {}
    for row in rows:
        component = {'arknet-core': 'core', 'arknet-tracker': 'tracker', 'arknet-agent': 'agent', 'arknet-transfer': 'transfer', 'arknet-ui': 'ui'}.get(row['Service'])
        if component:
            current = ssh(host, 'sudo docker inspect ' + row['Name'] + ' --format "{{.Image}}"').decode().strip()
            assert current == build['images'][component]['id']
            images[component] = current
        for publication in row.get('Publishers') or []:
            if publication.get('PublishedPort'):
                assert publication['URL'] == '127.0.0.1', 'Container port exposed beyond loopback'
    if machine['role'] == 'node':
        state = request('https://node-' + host[-1] + '.nodenet.arknet.media/api/v1/status')[1]
        assert state['state'] == 'CONNECTED' and state['jellyfin_healthy'] and state['database_healthy']
        policy = request('https://node-' + host[-1] + '.nodenet.arknet.media/api/v1/settings/peer-serving')[1]
        assert policy['enabled'] is False
        proof['machines'][host] = {'all_containers_healthy': True, 'state': state['state'],
                                  'peer_serving_enabled': False, 'release_manifest_available': state.get('release') is not None, 'image_ids': images}
    else:
        assert request('https://core.nodenet.arknet.media/health')[1]['healthy']
        proof['machines'][host] = {'all_containers_healthy': True, 'image_ids': images}
    prefs_code = "import subprocess,json; p=json.loads(subprocess.check_output(['tailscale','debug','prefs'])); print(json.dumps({'mode':p['NetfilterMode']}))"
    mode = json.loads(ssh(host, 'sudo python3 -c ' + shlex.quote(prefs_code)))['mode']
    assert mode == 0, 'Tailscale netfilter overrides UFW policy'
    proof['machines'][host]['running_container_count'] = len(rows)
    proof['machines'][host]['ufw_authoritative'] = True
    for port in ([22, 8083, 6969] if machine['role'] == 'core' else [22, 8082, 8086, 5174, 8096, 8084]):
        try:
            with socket.create_connection((machine['lan_ip'], port), timeout=.5):
                raise AssertionError(f'{host}: private port {port} reachable on LAN')
        except OSError:
            pass
    proof['boundaries'][host + '_private_ports_blocked_on_lan'] = True
    if machine['role'] == 'node':
        domain = 'jellyfin-' + host[-1] + '.nodenet.arknet.media'
        r = subprocess.run(['/usr/bin/curl', '-fsS', '--max-time', '10', '--resolve', domain + ':443:' + machine['lan_ip'],
                            'https://' + domain + '/health'], capture_output=True)
        assert r.returncode == 0, 'Jellyfin LAN access failed'
        forbidden_domain = 'node-' + host[-1] + '.nodenet.arknet.media'
        r = subprocess.run(['/usr/bin/curl', '-sS', '--max-time', '10', '--resolve', forbidden_domain + ':443:' + machine['lan_ip'],
                            '-o', '/dev/null', '-w', '%{http_code}', 'https://' + forbidden_domain + '/api/v1/status'], capture_output=True, text=True)
        assert r.returncode == 0 and r.stdout in ('403', '404'), 'Management leaked through LAN vhost'
        proof['boundaries'][host + '_lan_jellyfin_only'] = True
    print('SERVICE_IMAGE_AND_BOUNDARY_PASS', host, flush=True)
for domain, address, path, expected in [('node-b.nodenet.arknet.media', '100.64.0.23', '/api/v1/status', '403'),
                                         ('member-b.nodenet.arknet.media', '100.64.0.23', '/health', '200'),
                                         ('member-b.nodenet.arknet.media', '100.64.0.23', '/api/v1/status', '404'),
                                         ('core.nodenet.arknet.media', '100.64.0.21', '/api/admin/v1/nodes/pending', '403'),
                                        ('jellyfin-b.nodenet.arknet.media', '100.64.0.23', '/health', '200')]:
    result = ssh('nodenet-node-a', 'curl -sS --max-time 10 --resolve ' + domain + ':443:' + address +
                 ' -o /dev/null -w "%{http_code}" https://' + domain + path).decode().strip()
    assert result == expected, 'Node-to-Node/Core administrative boundary failed'
ssh_probe = '''import socket
for host in ('100.64.0.21','100.64.0.23'):
 try:
  s=socket.create_connection((host,22),timeout=2); s.close()
 except OSError:
  continue
 raise RuntimeError('Cross-machine SSH unexpectedly reachable')
print('BLOCKED')
'''
assert ssh('nodenet-node-a', 'python3 -c ' + shlex.quote(ssh_probe)).decode().strip() == 'BLOCKED'
proof['boundaries']['node_a_denied_cross_machine_ssh'] = True
proof['boundaries']['node_a_denied_node_b_management'] = True
proof['boundaries']['node_a_denied_core_administration'] = True
proof['boundaries']['node_a_can_reach_user_facing_jellyfin_b'] = True
(HERE / 'deployment-proof.json').write_text(json.dumps(proof, indent=2) + '\n')
print('DEPLOYMENT_AND_ACCESS_BOUNDARY_PROOF_PASS', flush=True)
