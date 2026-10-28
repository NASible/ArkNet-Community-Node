#!/usr/bin/env python3
"""Root-only, per-VM encrypted DB/config backup; media is intentionally excluded."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

os.umask(0o077)
config = json.loads(Path('/etc/arknet/backup.json').read_text())
project, database = config['project'], config['database']
compose = ['docker', 'compose', '--env-file', '/opt/arknet/runtime.env', '-f', '/opt/arknet/compose.json']
root = Path('/var/backups/arknet')
root.mkdir(mode=0o700, exist_ok=True)
root.chmod(0o700)
stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
output = root / (stamp + '.tar.age')
partial = output.with_suffix('.age.partial')
with tempfile.TemporaryDirectory(dir=root, prefix='.work-') as temporary:
    work = Path(temporary)
    with (work / 'database.dump').open('wb') as stream:
        subprocess.run(compose + ['exec', '-T', 'postgres', 'pg_dump', '-U', 'arknet', '-d', database, '-Fc'], stdout=stream, check=True)
    with tarfile.open(work / 'host-config.tar', 'w') as archive:
        for source in ['/opt/arknet/compose.json', '/opt/arknet/compose.yaml', '/opt/arknet/ui-nginx.conf', '/opt/arknet/runtime.env', '/opt/arknet/release.json',
                       '/etc/arknet', '/etc/ufw', '/etc/default/ufw', '/etc/fstab', '/etc/nginx/sites-available/arknet-applications',
                       '/etc/nginx/sites-enabled/arknet-applications', '/etc/systemd/system/nginx.service.d/arknet-vpn.conf',
                       '/etc/systemd/system/arknet-dns.service', '/etc/systemd/system/arknet-backup.service',
                       '/etc/systemd/system/arknet-backup.timer', '/usr/local/sbin/arknet-backup']:
            if Path(source).exists():
                archive.add(source, arcname=source.lstrip('/'))
    if config['role'] == 'node':
        volume = json.loads(subprocess.check_output(['docker', 'volume', 'inspect', project + '_appdata']))[0]['Mountpoint']
        # SQLite/config files must be copied with the Jellyfin writer stopped.
        subprocess.run(compose + ['stop', '-t', '30', 'jellyfin'], check=True, stdout=subprocess.DEVNULL)
        try:
            with tarfile.open(work / 'jellyfin-config.tar', 'w') as archive:
                archive.add(volume, arcname='jellyfin-config')
        finally:
            subprocess.run(compose + ['start', 'jellyfin'], check=True, stdout=subprocess.DEVNULL)
    (work / 'metadata.json').write_text(json.dumps({'project': project, 'database': database,
        'created_at': stamp, 'media_included': False, 'format': 'nodenet-config-backup/v1'}))
    process = subprocess.Popen(['age', '-r', config['recipient'], '-o', str(partial)], stdin=subprocess.PIPE)
    try:
        with tarfile.open(fileobj=process.stdin, mode='w|') as archive:
            for source in sorted(work.iterdir()):
                archive.add(source, arcname=source.name)
        process.stdin.close()
        if process.wait() != 0:
            raise RuntimeError('Backup encryption failed')
        partial.replace(output)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        partial.unlink(missing_ok=True)
output.chmod(0o600)
digest = hashlib.sha256(output.read_bytes()).hexdigest()
output.with_suffix('.age.sha256').write_text(digest + '  ' + output.name + '\n')
# Retention touches only this service's timestamped encrypted archives.
archives = sorted(root.glob('????????T??????Z.tar.age'))
for old in archives[:-7]:
    old.unlink()
    old.with_suffix('.age.sha256').unlink(missing_ok=True)
print(json.dumps({'backup': output.name, 'project': project, 'encrypted': True,
                  'bytes': output.stat().st_size, 'sha256': digest}))
