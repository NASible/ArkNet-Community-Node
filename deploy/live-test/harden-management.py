#!/usr/bin/env python3
"""Let UFW enforce this pilot's explicit rules instead of Tailscale's ACCEPT.

Only these four new VMs are changed, not Headscale or shared routes/ACLs. Each
change has an automatic rollback; cancel it only after a fresh Mac SSH succeeds.
"""
import shlex
from ops import MACHINES, ssh
from private_api import request

for host, machine in MACHINES.items():
    rules = ('sudo ufw allow in on tailscale0 proto tcp from 100.64.0.10 to any port 22 && '
             'sudo ufw allow in on tailscale0 proto tcp to any port 443 && ')
    if machine['role'] == 'core':
        rules += 'sudo ufw allow in on tailscale0 to any port 53 && '
    rules += 'sudo ufw delete allow in on tailscale0 && '
    command = ('test "${SSH_CONNECTION%% *}" = 100.64.0.10 && ' + rules +
               'sudo systemd-run --unit=arknet-netfilter-rollback --on-active=90s '
               '/usr/bin/env tailscale set --netfilter-mode=on && '
               'sudo tailscale set --netfilter-mode=off')
    ssh(host, command)
    # A distinct connection proves replacement access, not an established flow.
    state = ssh(host, 'systemctl is-active tailscaled && sudo ufw status')
    if b'Status: active' not in state:
        raise RuntimeError('Firewall validation failed; timed rollback left active')
    url = 'https://core.nodenet.arknet.media/health' if machine['role'] == 'core' else 'https://node-' + host[-1] + '.nodenet.arknet.media/api/v1/status'
    request(url)
    ssh(host, 'sudo systemctl stop arknet-netfilter-rollback.timer')
    print('UFW_AUTHORITATIVE_FRESH_MAC_ACCESS_VERIFIED', host, flush=True)
code = '''import socket,json
out={}
for host in ('100.64.0.21','100.64.0.23'):
 try:
  s=socket.create_connection((host,22),timeout=2); s.close(); out[host]='REACHABLE'
 except OSError: out[host]='BLOCKED'
assert all(value=='BLOCKED' for value in out.values())
print(json.dumps(out))
'''
print('NODE_A_CROSS_MACHINE_SSH_BLOCKED', ssh('nodenet-node-a', 'python3 -c ' + shlex.quote(code)).decode().strip())
