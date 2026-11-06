"""Harness-only bounded 429 retries; never retry denials or offline responses."""
import time


def retry_429(operation, seconds=45, max_attempts=8):
    """Return the last response tuple unchanged; re-POST only after Retry-After.

    Core sends delta-seconds. Missing/malformed hints get a one-second fallback;
    a zero hint gets a one-second floor to avoid busy loops. No retry starts past
    the monotonic deadline. Each operation must also have its own I/O timeout.
    """
    deadline = time.monotonic() + seconds
    for attempt in range(max_attempts):
        response = operation()
        if response[0] != 429 or attempt + 1 == max_attempts:
            return response
        value = next((value for name, value in response[1].items() if name.lower() == "retry-after"), "1")
        try:
            delay = max(1.0, float(int(value)))
        except (ValueError, TypeError, OverflowError):
            delay = 1.0
        if time.monotonic() + delay >= deadline:
            return response
        time.sleep(delay)
        if time.monotonic() >= deadline:
            return response
    raise ValueError("max_attempts must be positive")
