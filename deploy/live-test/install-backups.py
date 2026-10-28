#!/usr/bin/env python3
"""Install per-VM backup timers. Decryption identities stay on the operator Mac."""
import argparse
import json
import os
from pathlib import Path
from ops import HERE, MACHINES, put, ssh

parser = argparse.ArgumentParser()
parser.add_argument('--run', action='store_true')
args = parser.parse_args()
keys = Path.home() / '.local/share/arknet-live-test/backup-keys'
keys.mkdir(parents=True, mode=0o700, exist_ok=True)
keys.chmod(0o700)
for index, (host, machine) in enumerate(MACHINES.items()):
    keyfile = keys / (host + '.agekey')
    if not keyfile.exists():
        private = ssh(machine, 'age-keygen')
        if b'AGE-SECRET-KEY-' not in private:
            raise RuntimeError('Unexpected age identity output; output withheld')
        fd = os.open(keyfile, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'wb') as stream:
            stream.write(private)
    public = ssh(machine, 'age-keygen -y', keyfile.read_bytes()).decode().strip()
    assert public.startswith('age1') and '\n' not in public
    config = {'project': host, 'role': machine['role'], 'recipient': public,
              'database': 'arknet_core' if machine['role'] == 'core' else 'arknet_node'}
    put(machine, '/etc/arknet/backup.json', json.dumps(config) + '\n', 0o600)
    put(machine, '/usr/local/sbin/arknet-backup', (HERE / 'backup-vm.py').read_bytes(), 0o700)
    put(machine, '/etc/systemd/system/arknet-backup.service',
        '[Unit]\nDescription=Encrypted NodeNet database and local configuration backup\nAfter=docker.service\nRequires=docker.service\n'
        '[Service]\nType=oneshot\nExecStart=/usr/local/sbin/arknet-backup\nUMask=0077\nTimeoutStartSec=10min\n')
    minute = 15 + index * 10
    put(machine, '/etc/systemd/system/arknet-backup.timer',
        '[Unit]\nDescription=Daily NodeNet encrypted backup\n[Timer]\nOnCalendar=*-*-* 04:' + str(minute) + ':00 America/Anchorage\n'
        'Persistent=true\nUnit=arknet-backup.service\n[Install]\nWantedBy=timers.target\n')
    ssh(machine, 'sudo systemctl daemon-reload && sudo systemctl enable --now arknet-backup.timer')
    if args.run:
        ssh(machine, 'sudo systemctl start arknet-backup.service')
    print('BACKUP_TIMER_INSTALLED', host, '04:' + str(minute), 'America/Anchorage', flush=True)
