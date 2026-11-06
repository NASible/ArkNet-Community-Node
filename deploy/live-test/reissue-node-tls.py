#!/usr/bin/env python3
"""Reissue one Node's TLS leaf when it lacks a name create-pki.py now lists.

create-pki.py preserves existing leaves, so leaves issued before the member-x
hosts existed (Prompt 45B member listener) never gained that SAN. This retires
only the named Node's Mac-side leaf (kept in a superseded/ folder), lets
create-pki.py issue a fresh one from the existing CA (the CA and the Mac's
trust are unchanged), installs it on that VM after backing up the previous
pair there, and reloads nginx. Private keys travel over SSH stdin only and are
never printed.
"""
import argparse
import datetime
import shlex
import subprocess
import sys
from pathlib import Path
from ops import HERE, MACHINES, put, ssh

parser = argparse.ArgumentParser()
parser.add_argument('host', choices=[h for h, m in MACHINES.items() if m['role'] == 'node'])
args = parser.parse_args()
label = args.host[-1]
required = {'node-' + label, 'member-' + label, 'jellyfin-' + label}
pki = Path.home() / '.local/share/arknet-live-test/pki' / args.host
crt = pki / 'server.crt'


def names(path):
    text = subprocess.run(['openssl', 'x509', '-in', str(path), '-noout', '-text'],
                          capture_output=True, text=True, check=True, timeout=30).stdout
    return {item.strip().removeprefix('DNS:').removesuffix('.nodenet.arknet.media')
            for line in text.splitlines() if 'DNS:' in line for item in line.split(',')}


if not required <= names(crt):
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    retired = pki / ('superseded-' + stamp)
    retired.mkdir(mode=0o700)
    for name in ('server.key', 'server.crt', 'server.csr', 'leaf.conf'):
        if (pki / name).exists():
            (pki / name).rename(retired / name)
    subprocess.run([sys.executable, str(HERE / 'create-pki.py')], check=True, stdout=subprocess.DEVNULL, timeout=120)
    print('MAC_LEAF_REISSUED', args.host, 'previous kept in', retired.name, flush=True)
if not required <= names(crt):
    raise RuntimeError('Reissued leaf still lacks a required name')
machine = MACHINES[args.host]
stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
ssh(machine, 'sudo install -d -m 0700 /etc/arknet/tls/superseded-' + stamp +
    ' && sudo cp -a /etc/arknet/tls/server.key /etc/arknet/tls/server.crt /etc/arknet/tls/superseded-' + stamp + '/')
put(machine, '/etc/arknet/tls/server.key.new', (pki / 'server.key').read_bytes(), 0o600)
put(machine, '/etc/arknet/tls/server.crt.new', crt.read_bytes(), 0o644)
# Swap both files, then validate; restore the previous pair if nginx rejects it.
# /etc/arknet/tls is root-only (0700), so the whole swap runs in one root shell.
swap = ('set -e; cd /etc/arknet/tls; mv server.key.new server.key; mv server.crt.new server.crt; '
        'if nginx -t -q; then systemctl reload nginx; else '
        'cp -a superseded-' + stamp + '/server.key superseded-' + stamp + '/server.crt .; systemctl reload nginx; exit 1; fi')
ssh(machine, 'sudo sh -c ' + shlex.quote(swap))
print('NODE_TLS_LEAF_INSTALLED', args.host, sorted(required), flush=True)
