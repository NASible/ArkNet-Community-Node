"""Sanitized failure diagnostics bundles for the Docker acceptance harnesses.

A harness collects named text sections (API status, transfer state, worker
events, service log tails) just before cleanup and writes them here. Every
section passes through ``redact`` and the finished bundle is re-scanned: a
bundle that still contains a known secret is deleted and reported as an error
rather than left on disk.
"""
from __future__ import annotations

import json
import os
import re
import shutil
import tempfile
import traceback
from pathlib import Path
from typing import Callable, Iterable, Mapping

REDACTED = "[REDACTED]"
MAX_SECTION_CHARS = 200_000

# Capability URLs carry bearer grants as path segments. Redact the grant even
# when it is not in the harness's known-secret list (for example a grant minted
# by Core during the run).
_GRANT = r"[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}"
_PATTERNS = (
    (re.compile(r"(/announce/)" + _GRANT), r"\1<grant>"),
    (re.compile(r"(/api/(?:web-seed|torrent)/v1/media/media_[0-9a-f]{64}/)" + _GRANT), r"\1<grant>"),
    (re.compile(r"(?i)(authorization:?\s*\"?(?:bearer|mediabrowser token=)\s*\"?)[^\s\"',}]+"), r"\1" + REDACTED),
    (re.compile(r"(?i)(\b(?:api_key|token|password|access_token)=)[^&\s\"']+"), r"\1" + REDACTED),
)


def redact(text: str, secrets: Iterable[str], temporary: str | os.PathLike | None = None) -> str:
    """Remove known secret values, grant-bearing URL segments and the work path."""
    for value in sorted({s for s in secrets if s}, key=len, reverse=True):
        text = text.replace(value, REDACTED)
    for pattern, replacement in _PATTERNS:
        text = pattern.sub(replacement, text)
    if temporary:
        text = text.replace(str(temporary), "[TEMPORARY]")
    return text


def collect(collectors: Mapping[str, Callable[[], object]]) -> dict[str, str]:
    """Run each collector independently; one failing collector never hides the rest.

    Collector errors are recorded by exception type only, because exception
    messages can echo URLs or credentials.
    """
    sections = {}
    for name, collector in collectors.items():
        try:
            value = collector()
            if not isinstance(value, str):
                value = json.dumps(value, indent=2, sort_keys=True, default=str)
            sections[name] = value
        except Exception as error:  # diagnostics must never mask the original failure
            sections[name] = f"<collector failed: {type(error).__name__}>"
    return sections


def default_directory(run_id: str, results: str | None = None) -> Path:
    """Next to the results file when given, otherwise under the system temp dir."""
    if results:
        return Path(results).expanduser().resolve().parent / (run_id + "-diagnostics")
    return Path(tempfile.gettempdir()) / "arknet-diagnostics" / run_id


def failure_text(error: BaseException | None) -> str:
    if error is None:
        return "no exception; one or more acceptance criteria failed"
    return "".join(traceback.format_exception(type(error), error, error.__traceback__))


def write_bundle(directory: str | os.PathLike, sections: Mapping[str, str], secrets: Iterable[str],
                 temporary: str | os.PathLike | None = None) -> Path:
    """Write redacted sections as ``<name>.txt`` plus ``index.json``; verify, then return the path."""
    secrets = [s for s in secrets if s]
    target = Path(directory)
    target.mkdir(parents=True, exist_ok=True, mode=0o700)
    target.chmod(0o700)
    index = {}
    for name, text in sections.items():
        safe = re.sub(r"[^A-Za-z0-9_.-]+", "-", name).strip("-.") or "section"
        cleaned = redact(text, secrets, temporary)
        if len(cleaned) > MAX_SECTION_CHARS:
            cleaned = "[truncated to last %d chars]\n" % MAX_SECTION_CHARS + cleaned[-MAX_SECTION_CHARS:]
        path = target / (safe + ".txt")
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as stream:
            stream.write(cleaned)
        index[safe] = {"bytes": len(cleaned.encode())}
    (target / "index.json").write_text(json.dumps({"sections": index}, indent=2, sort_keys=True) + "\n")
    leaked = scan(target, secrets)
    if leaked:
        shutil.rmtree(target, ignore_errors=True)
        raise RuntimeError(f"diagnostics bundle contained {leaked} known secret value(s); bundle removed")
    return target


def scan(directory: str | os.PathLike, secrets: Iterable[str]) -> int:
    """Count known secret values present anywhere in a bundle. Never returns the values."""
    secrets = [s for s in secrets if s]
    found = 0
    for path in Path(directory).rglob("*"):
        if path.is_file():
            data = path.read_text(errors="replace")
            found += sum(1 for value in secrets if value in data)
    return found
