"""Measure that a revoked Node's transfer stops moving bytes (Prompt 47).

``RxSampler`` reads the summed non-loopback ``rx_bytes`` of one container a few
times a second, timestamped on the host. ``analyze`` turns those samples into
a verdict: the transfer was really receiving when Core revoked the Node, its
counters stopped within the stated bound, and they stayed quiet afterwards.
Standard library only.
"""
from __future__ import annotations

import re
import subprocess
import threading
import time

BLOCK = 16 * 1024          # one BitTorrent block: smaller steps are control traffic
RESIDUAL_LIMIT = 64 * 1024  # total allowed after the stop (API polling, announces)
QUIET_SECONDS = 10.0
ACTIVE_RATE = 64 * 1024     # bytes/s before revocation that count as "receiving"

_LOOP = """
import glob, sys, time
end = time.time() + float(sys.argv[1])
while time.time() < end:
    total = 0
    for path in glob.glob('/sys/class/net/*/statistics/rx_bytes'):
        if path.split('/')[4] != 'lo':
            total += int(open(path).read())
    print(total, flush=True)
    time.sleep(0.25)
"""


class RxSampler:
    """Samples a container's received bytes until ``stop`` (or ``seconds``)."""

    def __init__(self, container_id: str, seconds: float):
        self.samples: list[tuple[float, int]] = []
        self._process = subprocess.Popen(["docker", "exec", container_id, "python3", "-u", "-c", _LOOP, str(seconds)],
                                         stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        self._reader = threading.Thread(target=self._read, daemon=True)
        self._reader.start()

    def _read(self):
        assert self._process.stdout is not None
        for line in self._process.stdout:
            if line.strip().isdigit():
                self.samples.append((time.time(), int(line)))

    def stop(self) -> list[tuple[float, int]]:
        self._process.terminate()
        try:
            self._process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self._process.kill()
        self._reader.join(timeout=5)
        return list(self.samples)


def recent_rate(samples, seconds: float = 2.0) -> float:
    """Bytes/s over the last ``seconds`` of samples (0 without two samples)."""
    samples = list(samples)
    if len(samples) < 2:
        return 0
    window = [s for s in samples if s[0] >= samples[-1][0] - seconds]
    if len(window) < 2 or window[-1][0] == window[0][0]:
        return 0
    return (window[-1][1] - window[0][1]) / (window[-1][0] - window[0][0])


def analyze(samples, revoked_at: float, bound: float, quiet_seconds: float = QUIET_SECONDS) -> dict:
    """Verdict for (host_time, rx_bytes) samples; raises AssertionError.

    Make ``quiet_seconds`` longer than any pause a source can take on its own
    (such as a web-seed retry wait), or a stalled-then-resuming transfer would
    look stopped."""
    samples = sorted(samples)
    before = [s for s in samples if revoked_at - 3 <= s[0] <= revoked_at]
    if len(before) < 2 or before[-1][0] == before[0][0]:
        raise AssertionError("not receiving at revocation: too few samples before it")
    rate_before = (before[-1][1] - before[0][1]) / (before[-1][0] - before[0][0])
    if rate_before < ACTIVE_RATE:
        raise AssertionError(f"not receiving at revocation ({rate_before:.0f} B/s); the step proves nothing")
    stop_at = revoked_at
    for previous, current in zip(samples, samples[1:]):
        if current[0] > revoked_at and current[1] - previous[1] >= BLOCK:
            stop_at = current[0]
    stopped_after = stop_at - revoked_at
    if stopped_after > bound:
        raise AssertionError(f"byte counters did not stop within {bound:.0f}s of revocation"
                             f" (last block {stopped_after:.1f}s after it)")
    after = [s for s in samples if s[0] >= stop_at]
    quiet = after[-1][0] - stop_at if after else 0.0
    if quiet < quiet_seconds:
        raise AssertionError(f"quiet window too short to judge ({quiet:.1f}s < {quiet_seconds:.0f}s)")
    residual = after[-1][1] - after[0][1]
    if residual >= RESIDUAL_LIMIT:
        raise AssertionError(f"residual {residual} bytes received after the stop")
    received_after_revocation = after[0][1] - max(s[1] for s in samples if s[0] <= revoked_at)
    return {"rate_before": rate_before, "stopped_after": stopped_after, "quiet_seconds": quiet,
            "residual_bytes": residual, "received_after_revocation": received_after_revocation}


_COUNTS = re.compile(rb"8:completei(\d+)e10:incompletei(\d+)e")


def announce_counts(body: bytes) -> tuple[int, int]:
    """(complete, incomplete) from a compact tracker announce response."""
    match = _COUNTS.search(body)
    if body.startswith(b"d14:failure reason") or match is None:
        raise AssertionError("tracker announce failed")
    return int(match.group(1)), int(match.group(2))
