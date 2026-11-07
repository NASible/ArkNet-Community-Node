#!/usr/bin/env python3
"""Reboot only the four pilot VMs, one at a time, and verify new boot IDs/health.

Each VM is rebooted from its own OS over the pinned operator SSH connection
(`systemctl reboot`); no Proxmox credential is needed. Recovery requires a new
boot ID, active nginx, every Compose container running and healthy and, on the
Core VM, the relay socket proxy listening again. Run verify-deployment.py
afterwards for the full boundary checks (relay TLS/pin from each Node).
"""
import json
import shlex
import subprocess
import time
from ops import HERE, MACHINES, ssh

results = {}
for host, machine in MACHINES.items():
    core = machine['role'] == 'core'
    command = '''import subprocess,json
from pathlib import Path
ids=subprocess.check_output(['docker','ps','-aq','--filter','label=com.docker.compose.project=HOST']).decode().split()
containers=json.loads(subprocess.check_output(['docker','inspect',*ids])) if ids else []
active=lambda unit: subprocess.run(['systemctl','is-active','--quiet',unit]).returncode==0
print(json.dumps({'boot':Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
 'containers':[{'running':x['State']['Running'],'health':x['State'].get('Health',{}).get('Status')} for x in containers],
 'nginx':active('nginx'),'relay_proxy':active('arknet-relay-proxy.socket')}))
'''.replace('HOST', host)
    probe = 'sudo python3 -c ' + shlex.quote(command)
    old = json.loads(ssh(machine, probe))['boot']
    # The SSH session ends as the VM goes down; a non-zero exit is expected.
    try:
        ssh(machine, 'sudo systemd-run --on-active=2s /bin/systemctl reboot', timeout=30)
    except (RuntimeError, subprocess.TimeoutExpired):
        pass
    count = 4 if core else 5  # Core: postgres, core, tracker, relay
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        try:
            state = json.loads(ssh(machine, probe, timeout=15))
            if (state['boot'] != old and state['nginx'] and (state['relay_proxy'] or not core) and
                    len(state['containers']) == count and
                    all(c['running'] and c['health'] == 'healthy' for c in state['containers'])):
                break
        except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired):
            pass
        time.sleep(3)
    else:
        raise RuntimeError('Reboot recovery verification timed out: ' + host)
    results[host] = {'old_boot_id': old, 'new_boot_id': state['boot'], 'new_boot_verified': True,
                     'all_containers_healthy_after_vm_reboot': True, 'container_count': count,
                     'nginx_active_after_vm_reboot': True}
    if core:
        results[host]['relay_proxy_listening_after_vm_reboot'] = True
    (HERE / 'reboot-proof.json').write_text(json.dumps(results, indent=2) + '\n')
    print('REAL_VM_REBOOT_AND_STACK_RECOVERY_PASS', host, flush=True)
print('ALL_FOUR_VM_REBOOTS_RECOVERED', flush=True)
