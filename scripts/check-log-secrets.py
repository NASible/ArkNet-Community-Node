#!/usr/bin/env python3
"""Refuse to publish gate logs that contain secret-shaped values.

CI artifacts of a public repository are public. The harnesses already redact
their output; this is the last check before upload. Matches are reported by
file and kind only, never by value.

usage: scripts/check-log-secrets.py LOG_DIR
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

PATTERNS = {
    "capability": re.compile(rb"[A-Za-z0-9_-]{35,}\.[A-Za-z0-9_-]{35,}"),
    "admin-token": re.compile(rb"arknet_adm_[A-Za-z0-9_-]{8,}"),
    "private-key": re.compile(rb"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
    "setup-code": re.compile(rb"\b[A-Z2-7]{4}(?:-[A-Z2-7]{4}){7}\b"),
    "database-password": re.compile(rb"postgres(?:ql)?://[^:/@\s]+:(?!\*{3}@)[^@\s]{6,}@"),
}


def main(argv: list[str]) -> int:
    if len(argv) != 1 or not Path(argv[0]).is_dir():
        print("usage: check-log-secrets.py LOG_DIR (an existing directory)")
        return 2
    root = Path(argv[0])
    files = sorted(p for p in root.rglob("*") if p.is_file())
    hits = []
    for path in files:
        data = path.read_bytes()
        hits += [f"{path.relative_to(root)}: {kind}" for kind, pattern in PATTERNS.items() if pattern.search(data)]
    for hit in hits:
        print(hit)
    if hits:
        print(f"{len(hits)} secret-shaped value(s) found; do not publish these logs")
        return 1
    print(f"no secrets found in {len(files)} file(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
