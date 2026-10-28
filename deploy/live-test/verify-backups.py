#!/usr/bin/env python3
"""Verify off-VM backup decryption, PostgreSQL restore, and Jellyfin SQLite files.

The age identity never leaves this Mac. Restore uses a disposable database on
its original VM, never replacing a live database or mixing Node data into Core.
"""
import io
import json
from pathlib import Path, PurePosixPath
import secrets
import shlex
import sqlite3
import subprocess
import tarfile
import tempfile
from ops import HERE, MACHINES, ssh

base = Path.home() / '.local/share/arknet-live-test'
results = {}
for host, machine in MACHINES.items():
    candidates = sorted((base / 'backups' / host).glob('*.tar.age'))
    if not candidates:
        raise RuntimeError('No off-VM backup for ' + host)
    latest = candidates[-1]
    identity = base / 'backup-keys' / (host + '.agekey')
    decrypted = subprocess.run(['/opt/homebrew/bin/age', '-d', '-i', str(identity), str(latest)],
                               capture_output=True, check=True).stdout
    with tarfile.open(fileobj=io.BytesIO(decrypted), mode='r:') as archive:
        metadata = json.load(archive.extractfile('metadata.json'))
        assert metadata['project'] == host
        dump = archive.extractfile('database.dump').read()
        assert dump.startswith(b'PGDMP')
        jelly = archive.extractfile('jellyfin-config.tar').read() if machine['role'] == 'node' else None
    probe = 'nodenet_restore_probe_' + secrets.token_hex(4)
    postgres = host + '-postgres-1'
    prefix = 'sudo docker exec ' + postgres + ' '
    ssh(host, prefix + 'createdb -U arknet ' + probe)
    try:
        ssh(host, 'sudo docker exec -i ' + postgres + ' pg_restore --exit-on-error --no-owner --no-privileges -U arknet -d ' + probe, dump)
        sql = "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';"
        restored = ssh(host, prefix + 'psql -U arknet -d ' + probe + ' -At -c ' + shlex.quote(sql)).decode().strip()
        original = ssh(host, prefix + 'psql -U arknet -d ' + metadata['database'] + ' -At -c ' + shlex.quote(sql)).decode().strip()
        assert int(restored) > 0 and restored == original
    finally:
        ssh(host, prefix + 'dropdb -U arknet ' + probe)
    sqlite_count = 0
    if jelly:
        with tempfile.TemporaryDirectory(prefix='nodenet-restore-check-', dir='/Users/macbook/.hermes/cache/scratch') as temporary:
            directory = Path(temporary)
            with tarfile.open(fileobj=io.BytesIO(jelly), mode='r:') as archive:
                for member in archive:
                    path = PurePosixPath(member.name)
                    if member.isfile() and not path.is_absolute() and '..' not in path.parts and (
                        member.name.endswith('.db') or member.name.endswith('.db-wal')):
                        target = directory.joinpath(*path.parts)
                        target.parent.mkdir(parents=True, exist_ok=True)
                        target.write_bytes(archive.extractfile(member).read())
            for path in directory.rglob('*.db'):
                if not path.read_bytes().startswith(b'SQLite format 3\x00'):
                    continue
                # Rebuild platform-specific SHM on these disposable copies only.
                with sqlite3.connect(path) as connection:
                    assert connection.execute('PRAGMA quick_check').fetchall() == [('ok',)]
                sqlite_count += 1
        assert sqlite_count > 0, 'No Jellyfin SQLite database verified'
    results[host] = {'archive': latest.name, 'off_vm_copy_decrypted': True,
                     'postgresql_disposable_restore_passed': True, 'public_tables_restored': int(restored),
                     'jellyfin_sqlite_databases_checked': sqlite_count}
    (HERE / 'backup-proof.json').write_text(json.dumps(results, indent=2) + '\n')
    print('BACKUP_DECRYPT_AND_RESTORE_PASS', host, flush=True)
