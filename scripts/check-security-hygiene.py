#!/usr/bin/env python3
"""Reject secret-bearing and runtime artifacts in an ArkNet source tree."""

from __future__ import annotations

import os
import sys
from pathlib import Path

SKIP_DIRS = {".git", ".cache", "node_modules", "__pycache__"}
RUNTIME_SUFFIXES = (".db", ".db-shm", ".db-wal", ".sqlite", ".sqlite3")
SECRET_SUFFIXES = (".key", ".pem", ".p12", ".pfx")
SECRET_NAMES = {"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"}


def is_example_env(name: str) -> bool:
    return name == ".env.example" or name.endswith(".env.example") or (
        name.startswith(".env.") and name.endswith(".example")
    )


def forbidden_reason(path: Path, root: Path) -> str | None:
    relative = path.relative_to(root)
    name = path.name
    lowered = name.lower()
    if len(relative.parts) == 1 and lowered.startswith(".arknet-"):
        return "generated integration-test/runtime directory"
    if path.is_dir():
        return None
    if not is_example_env(lowered) and (lowered == ".env" or lowered.startswith(".env.") or lowered.endswith(".env")):
        return "non-example environment file"
    if lowered in SECRET_NAMES or lowered.endswith(SECRET_SUFFIXES):
        return "private key or certificate-key artifact"
    if lowered.endswith(RUNTIME_SUFFIXES):
        return "runtime database artifact"
    return None


def scan(root: Path) -> list[tuple[Path, str]]:
    violations: list[tuple[Path, str]] = []
    for current, dirs, files in os.walk(root):
        current_path = Path(current)
        kept_dirs: list[str] = []
        for name in dirs:
            candidate = current_path / name
            reason = forbidden_reason(candidate, root)
            if reason:
                violations.append((candidate.relative_to(root), reason))
            elif name not in SKIP_DIRS:
                kept_dirs.append(name)
        dirs[:] = kept_dirs
        for name in files:
            candidate = current_path / name
            reason = forbidden_reason(candidate, root)
            if reason:
                violations.append((candidate.relative_to(root), reason))
    return sorted(violations, key=lambda item: str(item[0]))


def main() -> int:
    root = Path(sys.argv[1] if len(sys.argv) > 1 else Path(__file__).resolve().parents[1]).resolve()
    violations = scan(root)
    if violations:
        print("Security hygiene check failed; remove these source-tree artifacts:", file=sys.stderr)
        for path, reason in violations:
            print(f"- {path}: {reason}", file=sys.stderr)
        return 1
    print("Security hygiene check passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
