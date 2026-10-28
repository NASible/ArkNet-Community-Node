#!/bin/sh
# Check the repository foundation without installing dependencies.
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

fail() {
    printf 'FAIL: %s\n' "$1" >&2
    exit 1
}

for directory in core node-agent node-ui transfer tracker deploy/core deploy/node schemas docs scripts; do
    [ -d "$directory" ] || fail "Missing directory: $directory"
    [ -s "$directory/README.md" ] || fail "Missing directory documentation: $directory/README.md"
done

[ -s README.md ] || fail 'Missing root README.md'
[ -s .gitignore ] || fail 'Missing .gitignore'
[ -f VERSION ] || fail 'Missing VERSION'
[ "$(tr -d '\n\r' < VERSION)" = '0.0.1-dev' ] || fail 'Expected VERSION to be 0.0.1-dev'

printf 'PASS: scaffold directories, documentation, ignore file, and version marker\n'
