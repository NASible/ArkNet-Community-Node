#!/usr/bin/env python3
"""Install private TLS identities and project-only DNS on the four new VMs.

Infrastructure health is intentionally distinct from application readiness.
No CA signing key is uploaded. No public/shared DNS is modified.
"""
from pathlib import Path
import base64
import io
import json
import shlex
import subprocess
import tarfile

HERE = Path(__file__).resolve().parent
PKI = Path.home() / '.local/share/arknet-live-test/pki'
INVENTORY = json.loads((HERE / 'inventory.json').read_text())


def ssh(machine, command, data=None):
    args = ['ssh', '-i', str(Path.home() / '.ssh/arknet-live-test'),
            '-o', 'IdentitiesOnly=yes', '-o', 'BatchMode=yes',
            '-o', 'StrictHostKeyChecking=yes',
            '-o', 'UserKnownHostsFile=' + str(Path.home() / '.ssh/arknet-live-test-known_hosts'),
            '-o', 'HostKeyAlias=' + machine['lan_ip'],
            'arknet@' + machine['tailscale_ip'], command]
    result = subprocess.run(args, input=data, capture_output=True, timeout=240)
    if result.returncode:
        # Never include a payload or key in error messages.
        raise RuntimeError(machine['hostname'] + ': remote operation failed: ' + result.stderr.decode(errors='replace')[-1200:])
    return result.stdout.decode(errors='replace')


def put(machine, path, content, mode=0o644):
    payload = json.dumps({'path': path, 'content': base64.b64encode(content).decode(), 'mode': mode}).encode()
    code = ('import json,sys,base64,os; from pathlib import Path; '
            'd=json.load(sys.stdin); p=Path(d["path"]); p.parent.mkdir(parents=True,exist_ok=True); '
            'fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,d["mode"]); '
            'os.write(fd,base64.b64decode(d["content"])); os.close(fd); p.chmod(d["mode"])')
    ssh(machine, 'sudo python3 -c ' + shlex.quote(code), payload)


for machine in INVENTORY['machines']:
    host = machine['hostname']
    bundle = io.BytesIO()
    with tarfile.open(fileobj=bundle, mode='w') as archive:
        for name, source in [('server.key', PKI / host / 'server.key'),
                             ('server.crt', PKI / host / 'server.crt'),
                             ('nodenet-root-ca.crt', PKI / 'nodenet-root-ca.crt')]:
            item = tarfile.TarInfo(name)
            data = source.read_bytes()
            item.size = len(data)
            item.mode = 0o600 if name.endswith('.key') else 0o644
            archive.addfile(item, io.BytesIO(data))
    ssh(machine, 'sudo install -d -m 0700 /etc/arknet/tls && '
        'sudo tar -xpf - -C /etc/arknet/tls && '
        'sudo install -m 0644 /etc/arknet/tls/nodenet-root-ca.crt /usr/local/share/ca-certificates/nodenet-root-ca.crt && '
        'sudo update-ca-certificates >/dev/null', bundle.getvalue())
    names = (['nodenet.arknet.media', 'core.nodenet.arknet.media', 'tracker.nodenet.arknet.media']
             if machine['role'] == 'core' else
             [host.removeprefix('nodenet-') + '.nodenet.arknet.media',
              'jellyfin-' + host[-1] + '.nodenet.arknet.media'])
    config = '''# Infrastructure TLS verification only; application deployment is pending.
server {
    listen ''' + machine['tailscale_ip'] + ''':443 ssl;
    server_name ''' + ' '.join(names) + ''';
    ssl_certificate /etc/arknet/tls/server.crt;
    ssl_certificate_key /etc/arknet/tls/server.key;
    ssl_protocols TLSv1.2 TLSv1.3;
    access_log off;
    error_log /var/log/nginx/arknet-error.log crit;
    location = /infrastructure/health {
        default_type text/plain;
        return 200 "NodeNet infrastructure TLS ready\\n";
    }
    location / {
        default_type text/plain;
        return 503 "ArkNet application deployment is pending.\\n";
    }
}
'''
    put(machine, '/etc/nginx/sites-available/arknet-infrastructure', config.encode())
    put(machine, '/etc/systemd/system/nginx.service.d/arknet-vpn.conf',
        b'[Unit]\nAfter=tailscaled.service\n\n[Service]\nRestart=on-failure\nRestartSec=5\n')
    print(ssh(machine, 'sudo rm -f /etc/nginx/sites-enabled/default && '
              'sudo ln -sfn /etc/nginx/sites-available/arknet-infrastructure /etc/nginx/sites-enabled/arknet-infrastructure && '
              'sudo nginx -t && sudo systemctl daemon-reload && sudo systemctl enable nginx >/dev/null && '
              'sudo systemctl restart nginx && systemctl is-active nginx').strip())
    print('TLS_IDENTITY_INSTALLED', host, flush=True)

core = next(machine for machine in INVENTORY['machines'] if machine['role'] == 'core')
ssh(core, 'sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq dnsmasq-base >/dev/null')
put(core, '/etc/arknet/dnsmasq.conf', (HERE / 'dnsmasq.conf').read_bytes())
put(core, '/etc/systemd/system/arknet-dns.service', (HERE / 'arknet-dns.service').read_bytes())
print(ssh(core, 'sudo dnsmasq --test --conf-file=/etc/arknet/dnsmasq.conf && '
          'sudo systemctl daemon-reload && sudo systemctl enable --now arknet-dns && '
          'systemctl is-active arknet-dns').strip())
print('PRIVATE_DNS_INSTALLED', core['tailscale_ip'], flush=True)
