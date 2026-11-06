#!/bin/sh
set -eu

# Request debug channels bypass Vite's sanitized logger and may expose URL
# grants or HTTP credentials. Clear them before Node initializes any modules
# (including Node's own HTTP diagnostics), not in server.mjs after imports.
# Normal Vite startup, build and sanitized proxy-error output stays enabled.
unset DEBUG NODE_DEBUG NODE_DEBUG_NATIVE
cd "$(dirname "$0")"
if [ "$#" -eq 0 ]; then
  set -- server.mjs
fi
exec node "$@"
