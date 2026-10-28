#!/bin/bash
# Run through the macOS administrator authorization dialog, never a chat password.
set -euo pipefail
[[ "$(id -u)" == 0 ]] || { printf 'Administrator authorization required.\n' >&2; exit 1; }
HERE=$(cd "$(dirname "$0")" && pwd)
CERT="$HERE/nodenet-root-ca.crt"
RESOLVER=/etc/resolver/nodenet.arknet.media
EXPECTED='8B:E6:FE:44:0F:57:67:FC:A8:E6:1E:6C:63:99:85:D4:34:64:AF:60:C5:6D:FD:DC:67:51:38:F0:3B:4B:45:2C'
ACTUAL=$(/usr/bin/openssl x509 -in "$CERT" -noout -fingerprint -sha256)
[[ "$ACTUAL" == "SHA256 Fingerprint=$EXPECTED" ]] || { printf 'CA fingerprint mismatch; refusing changes.\n' >&2; exit 1; }
if [[ -e "$RESOLVER" ]] && ! /usr/bin/cmp -s "$HERE/mac-resolver.conf" "$RESOLVER"; then
  printf 'A different project resolver already exists; refusing overwrite.\n' >&2
  exit 1
fi
# Restrict this root's explicit trust policy to SSL (not code signing/email).
/usr/bin/security add-trusted-cert -d -r trustRoot -p ssl \
  -k /Library/Keychains/System.keychain "$CERT"
/usr/bin/install -d -m 0755 /etc/resolver
/usr/bin/install -o root -g wheel -m 0644 "$HERE/mac-resolver.conf" "$RESOLVER"
/usr/bin/dscacheutil -flushcache
/usr/bin/killall -HUP mDNSResponder
printf 'MAC_PROJECT_CA_AND_SPLIT_DNS_INSTALLED\n'
