#!/usr/bin/env python3
"""Pull only encrypted NodeNet backups to this Mac, never into Core.

Installed outside Documents so a user LaunchAgent doesn't need Documents access.
Node-provided tar entries are validated, never blindly extracted.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tarfile

os.umask(0o077)
BASE = Path.home() / '.local/share/arknet-live-test'
settings = json.loads((BASE / 'backup-pull-settings.json').read_text())
pattern = re.compile(r'^\d{8}T\d{6}Z\.tar\.age(?:\.sha256)?$')
remote = '''from pathlib import Path
import re,sys,tarfile
root=Path('/var/backups/arknet')
pattern=re.compile(r'^\\d{8}T\\d{6}Z\\.tar\\.age(?:\\.sha256)?$')
with tarfile.open(fileobj=sys.stdout.buffer,mode='w|') as archive:
 for p in sorted(root.iterdir()):
  if p.is_file() and not p.is_symlink() and pattern.fullmatch(p.name):
   archive.add(p,arcname=p.name,recursive=False)
'''
for machine in settings['machines']:
    destination = BASE / 'backups' / machine['hostname']
    destination.mkdir(parents=True, mode=0o700, exist_ok=True)
    if shutil.disk_usage(destination).free < 5 * 1024**3:
        raise RuntimeError('Less than 5 GiB free; refusing backup pull')
    args = ['ssh', '-i', str(Path.home() / '.ssh/arknet-live-test'), '-o', 'IdentitiesOnly=yes',
            '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes',
            '-o', 'UserKnownHostsFile=' + str(Path.home() / '.ssh/arknet-live-test-known_hosts'),
            '-o', 'HostKeyAlias=' + machine['lan_ip'], 'arknet@' + machine['tailscale_ip'],
            'sudo python3 -c ' + shlex.quote(remote)]
    process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    received = 0
    try:
        with tarfile.open(fileobj=process.stdout, mode='r|') as archive:
            for member in archive:
                if not member.isfile() or not pattern.fullmatch(member.name) or member.size > 1024**3:
                    raise RuntimeError('Untrusted backup archive entry rejected')
                target = destination / member.name
                partial = destination / (member.name + '.partial')
                stream = archive.extractfile(member)
                with partial.open('wb') as output:
                    shutil.copyfileobj(stream, output, length=1024*1024)
                partial.replace(target)
                target.chmod(0o600)
                received += 1
        process.stdout.close()
        if process.wait(timeout=30) != 0:
            raise RuntimeError('Encrypted backup transfer failed; stderr withheld')
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
    for checksum in destination.glob('*.age.sha256'):
        digest, filename = checksum.read_text().split()
        if not pattern.fullmatch(filename) or not re.fullmatch('[0-9a-f]{64}', digest):
            raise RuntimeError('Invalid backup checksum manifest')
        local = destination / filename
        if local.exists():
            state = hashlib.sha256()
            with local.open('rb') as stream:
                for chunk in iter(lambda: stream.read(1024*1024), b''):
                    state.update(chunk)
            if state.hexdigest() != digest:
                raise RuntimeError('Encrypted backup checksum mismatch')
    for old in sorted(destination.glob('????????T??????Z.tar.age'))[:-14]:
        old.unlink()
        old.with_suffix('.age.sha256').unlink(missing_ok=True)
    print(json.dumps({'host': machine['hostname'], 'encrypted_files_received': received, 'checksums_verified': True}), flush=True)
