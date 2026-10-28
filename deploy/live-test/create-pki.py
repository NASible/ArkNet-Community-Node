#!/usr/bin/env python3
"""Generate the private NodeNet CA and per-machine TLS identities on the Mac.

Private keys never enter the repository or a VM other than their own leaf key.
Existing CA/key pairs are preserved; partial existing material causes a stop.
"""
import os
from pathlib import Path
import subprocess

os.umask(0o077)
BASE = Path.home() / '.local/share/arknet-live-test/pki'
BASE.mkdir(parents=True, mode=0o700, exist_ok=True)
BASE.chmod(0o700)
ROOT = BASE / 'nodenet-root-ca.crt'
KEY = BASE / 'nodenet-root-ca.key'


def run(*args):
    result = subprocess.run(['openssl', *map(str, args)], capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError(f'OpenSSL {args[0]} failed; private material/output withheld')
    return result.stdout


def consistent(*paths):
    flags = [path.exists() for path in paths]
    if any(flags) and not all(flags):
        raise RuntimeError('Partial existing PKI material; refusing overwrite')
    return all(flags)


if not consistent(KEY, ROOT):
    conf = BASE / 'ca.conf'
    conf.write_text('''[req]
prompt = no
distinguished_name = dn
x509_extensions = ca
[dn]
CN = NodeNet ArkNet Private Root CA
O = ArkNet Private Live Test
[ca]
basicConstraints = critical,CA:true,pathlen:0
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid:always
nameConstraints = critical,permitted;DNS:nodenet.arknet.media
''')
    run('ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', KEY)
    run('req', '-new', '-x509', '-sha256', '-days', '1095', '-key', KEY,
        '-config', conf, '-out', ROOT)
    print('CA_CREATED')
else:
    print('EXISTING_CA_PRESERVED')
KEY.chmod(0o600)

identities = {
    'nodenet-core': ['core.nodenet.arknet.media', 'tracker.nodenet.arknet.media', 'nodenet.arknet.media'],
    'nodenet-node-a': ['node-a.nodenet.arknet.media', 'jellyfin-a.nodenet.arknet.media'],
    'nodenet-node-b': ['node-b.nodenet.arknet.media', 'jellyfin-b.nodenet.arknet.media'],
    'nodenet-node-c': ['node-c.nodenet.arknet.media', 'jellyfin-c.nodenet.arknet.media'],
}
for host, names in identities.items():
    directory = BASE / host
    directory.mkdir(mode=0o700, exist_ok=True)
    key, crt, csr = [directory / ('server.' + suffix) for suffix in ('key', 'crt', 'csr')]
    if not consistent(key, crt):
        conf = directory / 'leaf.conf'
        conf.write_text('''[req]
prompt = no
distinguished_name = dn
req_extensions = leaf
[dn]
CN = ''' + names[0] + '''
[leaf]
basicConstraints = critical,CA:false
keyUsage = critical,digitalSignature,keyAgreement
extendedKeyUsage = serverAuth
subjectAltName = ''' + ','.join('DNS:' + name for name in names) + '\n')
        run('ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', key)
        run('req', '-new', '-sha256', '-key', key, '-config', conf, '-out', csr)
        run('x509', '-req', '-sha256', '-days', '90', '-in', csr, '-CA', ROOT,
            '-CAkey', KEY, '-set_serial', '0x' + os.urandom(16).hex(),
            '-extfile', conf, '-extensions', 'leaf', '-out', crt)
    key.chmod(0o600)
    run('verify', '-CAfile', ROOT, crt)
    run('x509', '-in', crt, '-checkend', '86400', '-noout')
    print('LEAF_CHAIN_VERIFIED', host)

public_copy = Path(__file__).resolve().parent / 'nodenet-root-ca.crt'
public_copy.write_bytes(ROOT.read_bytes())
public_copy.chmod(0o644)
print('PUBLIC_CA_EXPORTED', public_copy)
print(run('x509', '-in', ROOT, '-noout', '-subject', '-dates', '-fingerprint', '-sha256').strip())
