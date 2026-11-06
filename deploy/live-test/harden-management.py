#!/usr/bin/env python3
"""Let UFW enforce this pilot's explicit rules instead of Tailscale's ACCEPT.

Only these four new VMs are changed, not Headscale or shared routes/ACLs. Each
change has an automatic rollback; cancel it only after a fresh Mac SSH succeeds.
"""
import argparse
import json
import shlex
import socket
import sys
from ops import MACHINES, core_machine, put, ssh
from private_api import request

parser = argparse.ArgumentParser()
parser.add_argument('--relay', action='store_true',
                    help='only (re)apply the Core relay listener: Node VPN IPs -> relay, nothing else')
args = parser.parse_args()


def relay_exposure():
    """Expose the loopback-published relay on Core's VPN IP to the three Nodes only.

    A socket-activated systemd-socket-proxyd (no extra packages) binds the VPN
    address with FreeBind, so boot order against tailscaled cannot break it.
    UFW and the socket's own IP allowlist both admit only Node VPN IPs; the LAN
    address and every other tailnet peer (including the Mac) stay blocked.
    """
    core = core_machine()
    vpn = core['tailscale_ip']
    nodes = [m['tailscale_ip'] for m in MACHINES.values() if m['role'] == 'node']
    put(core, '/etc/systemd/system/arknet-relay-proxy.socket', f'''[Unit]
Description=ArkNet relay VPN listener (Node VPN IPs only)

[Socket]
ListenStream={vpn}:7443
FreeBind=true
IPAddressDeny=any
IPAddressAllow={' '.join(nodes)}

[Install]
WantedBy=sockets.target
''')
    put(core, '/etc/systemd/system/arknet-relay-proxy.service', '''[Unit]
Description=ArkNet relay VPN listener proxy to the loopback-published relay container
Requires=arknet-relay-proxy.socket
After=arknet-relay-proxy.socket

[Service]
ExecStart=/usr/lib/systemd/systemd-socket-proxyd --connections-max=512 127.0.0.1:7443
DynamicUser=yes
NoNewPrivileges=yes
CapabilityBoundingSet=
PrivateTmp=yes
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
IPAddressDeny=any
IPAddressAllow=localhost
''')
    rules = ' && '.join('sudo ufw allow in on tailscale0 proto tcp from ' + ip + ' to ' + vpn +
                        ' port 7443 comment ' + shlex.quote('ArkNet relay: Node VPN IP only') for ip in nodes)
    undo = '; '.join('ufw delete allow in on tailscale0 proto tcp from ' + ip + ' to ' + vpn + ' port 7443' for ip in nodes)
    undo += '; systemctl disable --now arknet-relay-proxy.socket arknet-relay-proxy.service'
    ssh(core, 'test \"${SSH_CONNECTION%% *}\" = 100.64.0.10 || exit 1; '
              'sudo systemctl reset-failed arknet-relay-rollback.timer arknet-relay-rollback.service 2>/dev/null; '
              'sudo systemd-run --unit=arknet-relay-rollback --on-active=120s /bin/sh -c ' + shlex.quote(undo) + ' && '
              + rules + ' && sudo systemctl daemon-reload && sudo systemctl enable --now arknet-relay-proxy.socket')
    # Fresh management access still works (a new connection, not an old flow).
    state = ssh(core, 'systemctl is-active arknet-relay-proxy.socket && sudo ufw status')
    if b'Status: active' not in state:
        raise RuntimeError('Firewall validation failed; timed relay rollback left active')
    probe = '''import socket,ssl,json
ctx=ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT); ctx.check_hostname=False; ctx.verify_mode=ssl.CERT_NONE
with socket.create_connection(('VPN',7443),timeout=5) as raw:
 with ctx.wrap_socket(raw,server_hostname='relay') as tls: print(json.dumps({'tls':tls.version()}))
'''.replace('VPN', vpn)
    for host, machine in MACHINES.items():
        if machine['role'] == 'node':
            result = json.loads(ssh(host, 'python3 -c ' + shlex.quote(probe), timeout=30))
            assert result['tls'] == 'TLSv1.3', host + ': relay did not negotiate TLS 1.3'
            print('RELAY_REACHABLE_FROM_NODE_VPN', host, result['tls'], flush=True)
    # The operator Mac is not a relay client, and the LAN address never listens.
    for address in (vpn, core['lan_ip']):
        try:
            with socket.create_connection((address, 7443), timeout=3):
                raise RuntimeError('Relay unexpectedly reachable from the operator Mac at ' + address)
        except OSError:
            pass
    ssh(core, 'sudo systemctl stop arknet-relay-rollback.timer')
    policy = json.loads(ssh(core, 'cat /etc/arknet/network-policy.json'))
    policy['relay_listener'] = {'address': vpn + ':7443', 'allowed_sources': nodes,
                                'docker_publication': '127.0.0.1:7443', 'lan_listener': False}
    put(core, '/etc/arknet/network-policy.json', json.dumps(policy) + '\n')
    print('RELAY_LISTENER_NODE_VPN_ONLY_VERIFIED', flush=True)


if args.relay:
    relay_exposure()
    sys.exit(0)

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
    # Reachability only: Node management answers 401 without an operator session (Prompt 45B).
    request(url, expected=(200, 401))
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
