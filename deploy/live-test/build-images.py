#!/usr/bin/env python3
"""Upload a strict non-secret source allowlist and build immutable pilot images."""
from pathlib import Path
import hashlib
import io
import json
import shlex
import tarfile
from ops import ROOT, HERE, ssh

allowed = ['core', 'tracker', 'relay', 'transfer', 'node-agent', 'node-ui']
files = []
for folder in allowed:
    for path in sorted((ROOT / folder).rglob('*')):
        relative = path.relative_to(ROOT)
        if not path.is_file() or path.is_symlink():
            continue
        if any(part.startswith('.') or part in ('node_modules', 'dist', 'bin', '__pycache__') for part in relative.parts):
            continue
        # Exclude top-level runtime folders, not source packages such as
        # core/internal/media or node-agent/internal/media.
        if len(relative.parts) > 1 and relative.parts[1] in ('secrets', 'data', 'media'):
            continue
        if path.suffix.lower() in ('.key', '.pem', '.db', '.sqlite', '.log', '.env') or path.name.startswith('.env'):
            continue
        files.append((relative, path))
# Guard against accidentally excluding product source packages as runtime data.
assert any(rel.parts[:3] == ('core', 'internal', 'media') for rel, _ in files)
assert any(str(rel) == 'transfer/worker.py' for rel, _ in files)
assert any(str(rel) == 'relay/Dockerfile' for rel, _ in files)
hash_state = hashlib.sha256()
bundle = io.BytesIO()
with tarfile.open(fileobj=bundle, mode='w:gz') as archive:
    for relative, path in files:
        data = path.read_bytes()
        hash_state.update(str(relative).encode() + b'\x00' + data)
        item = tarfile.TarInfo(str(relative))
        item.mode, item.size = 0o644, len(data)
        archive.addfile(item, io.BytesIO(data))
revision = hash_state.hexdigest()
record = {'source_sha256': revision, 'images': {}}
# The operator-run relay (Prompt 48A, decision D5) runs beside the tracker on Core.
for host, components in [('nodenet-core', ['core', 'tracker', 'relay']), ('nodenet-node-a', ['agent', 'transfer', 'ui'])]:
    print('UPLOAD_ALLOWLIST', host, len(files), 'files', flush=True)
    ssh(host, 'sudo install -d -m 0755 /opt/arknet/source && sudo tar -xzpf - -C /opt/arknet/source', bundle.getvalue())
    for component in components:
        tag = 'nodenet-' + component + ':' + revision[:12]
        context = {'core': 'core', 'tracker': 'tracker', 'relay': 'relay', 'agent': 'node-agent', 'transfer': 'transfer', 'ui': 'node-ui'}[component]
        dockerfile = ''  # every image uses its component's shipped Dockerfile
        command = ('cd /opt/arknet/source && sudo docker build --label org.arknet.source.sha256=' + revision +
                   ' --build-arg GOMAXPROCS=2 -t ' + tag + dockerfile + ' ' + context)
        print('BUILD', host, component, tag, flush=True)
        try:
            ssh(host, 'sudo sh -c ' + shlex.quote(command + ' > /opt/arknet/build-' + component + '.log 2>&1'), timeout=900)
        except Exception:
            print('BUILD_FAILED_LOG', host, '/opt/arknet/build-' + component + '.log', flush=True)
            raise
        info = json.loads(ssh(host, 'sudo docker image inspect ' + tag + ' --format "{{json .}}"'))
        record['images'][component] = {'tag': tag, 'id': info['Id'], 'host': host}
        print('BUILT_VERIFIED', component, info['Id'], flush=True)
        (HERE / 'build-progress.json').write_text(json.dumps(record, indent=2) + '\n')
# Publish only a complete record: deployment must never read a half-built set.
(HERE / 'build-progress.json').replace(HERE / 'build-record.json')
print('ALL_IMAGES_BUILT', revision, flush=True)
