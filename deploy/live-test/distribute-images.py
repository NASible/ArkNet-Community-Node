#!/usr/bin/env python3
"""Copy the three built Node images, verifying exact image IDs after load."""
import gzip
import json
from pathlib import Path
import shlex
from ops import HERE, ssh

record = json.loads((HERE / 'build-record.json').read_text())
refs = [record['images'][component]['tag'] for component in ('agent', 'transfer', 'ui')]
# The images contain source/binaries only, never runtime.env or enrollment keys.
command = 'bash -o pipefail -c ' + shlex.quote('sudo docker save ' + ' '.join(refs) + ' | gzip -1')
bundle = ssh('nodenet-node-a', command, timeout=300)
print('EXPORTED_IMAGE_BUNDLE_BYTES', len(bundle), flush=True)
for host in ('nodenet-node-b', 'nodenet-node-c'):
    print(ssh(host, 'bash -o pipefail -c ' + shlex.quote('gunzip -c | sudo docker load'), bundle, timeout=300).decode())
    for component in ('agent', 'transfer', 'ui'):
        expected = record['images'][component]
        actual = ssh(host, 'sudo docker image inspect ' + expected['tag'] + ' --format "{{.Id}}"').decode().strip()
        assert actual == expected['id'], host + ': image ID mismatch'
    print('NODE_IMAGE_IDS_VERIFIED', host, flush=True)
