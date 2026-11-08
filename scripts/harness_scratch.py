"""Where test harnesses put temporary files.

Order: `ARKNET_SCRATCH` (created if needed), then the maintainer's Hermes
scratch directory if it already exists (macOS' per-user TMPDIR is not always
shared with Docker Desktop or Colima), then the system temporary directory.
CI runners and other machines use the system temporary directory.
"""
from __future__ import annotations

import os
import tempfile
from pathlib import Path


def scratch_root() -> Path:
    explicit = os.environ.get("ARKNET_SCRATCH")
    if explicit:
        path = Path(explicit)
        path.mkdir(parents=True, exist_ok=True)
        return path
    hermes = Path.home() / ".hermes" / "cache" / "scratch"
    if hermes.is_dir():
        return hermes
    return Path(tempfile.gettempdir())
