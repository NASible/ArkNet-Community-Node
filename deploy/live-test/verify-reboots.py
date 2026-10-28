#!/usr/bin/env python3
"""Reboot only the four new VMs, one at a time, and verify new boot IDs/health."""
import json
import shlex
import subprocess
import sys
import time
from pathlib import Path
from ops import HERE, MACHINES, ssh
sys.path.insert(0, '/Users/macbook/.hermes/cache/scratch')
from arknet_proxmox import Proxmox

api = Proxmox()
results = {}
for host, machine in MACHINES.items():
    command = '''import subprocess,json
from pathlib import Path
ids=subprocess.check_output(['docker','ps','-aq','--filter','label=com.docker.compose.project=HOST']).decode().split()
containers=json.loads(subprocess.check_output(['docker','inspect',*ids])) if ids else []
print(json.dumps({'boot':Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
 'containers':[{'running':x['State']['Running'],'health':x['State'].get('Health',{}).get('Status')} for x in containers],
 'nginx':subprocess.run(['systemctl','is-active','--quiet','nginx']).returncode==0}))
'''.replace('HOST', host)
    probe = 'sudo python3 -c ' + shlex.quote(command)
    old = json.loads(ssh(machine, probe))['boot']
    api.call('/nodes/pve2/qemu/' + str(machine['vmid']) + '/status/reboot', 'POST', {})
    deadline = time.monotonic() + 240
    while time.monotonic() < deadline:
        try:
            state = json.loads(ssh(machine, probe, timeout=15))
            count = 3 if machine['role'] == 'core' else 5
            if state['boot'] != old and state['nginx'] and len(state['containers']) == count and all(c['running'] and c['health'] == 'healthy' for c in state['containers']):
                break
        except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired):
            pass
        time.sleep(3)
    else:
        raise RuntimeError('Reboot recovery verification timed out: ' + host)
    results[host] = {'old_boot_id': old, 'new_boot_id': state['boot'], 'new_boot_verified': True,
                     'all_containers_healthy_after_vm_reboot': True, 'nginx_active_after_vm_reboot': True}
    (HERE / 'reboot-proof.json').write_text(json.dumps(results, indent=2) + '\n')
    print('REAL_VM_REBOOT_AND_STACK_RECOVERY_PASS', host, flush=True)
