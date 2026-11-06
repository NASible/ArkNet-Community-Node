"""Offline tests for bounded harness-only admission retries."""
import unittest
from unittest import mock

from scripts import rate_limit_retry as retry


class Retry429Tests(unittest.TestCase):
    def test_retries_only_429_and_preserves_final_denial(self):
        for final in (200, 303, 401, 403, 503):
            with self.subTest(final=final):
                operation = mock.Mock(side_effect=[(429, {"Retry-After": "6"}, b"limited"),
                                                   (final, {}, b"authoritative")])
                with mock.patch.object(retry.time, "sleep") as sleep:
                    result = retry.retry_429(operation)
                self.assertEqual(result, (final, {}, b"authoritative"))
                self.assertEqual(operation.call_count, 2)
                sleep.assert_called_once_with(6.0)

    def test_denials_and_offline_are_never_retried(self):
        for status in (401, 403, 503):
            operation = mock.Mock(return_value=(status, {}, b"unchanged"))
            with mock.patch.object(retry.time, "sleep") as sleep:
                self.assertEqual(retry.retry_429(operation)[0], status)
            operation.assert_called_once_with()
            sleep.assert_not_called()

    def test_deadline_returns_last_429_without_early_retry(self):
        operation = mock.Mock(return_value=(429, {"retry-after": "60"}, b"limited"))
        with mock.patch.object(retry.time, "sleep") as sleep:
            self.assertEqual(retry.retry_429(operation, seconds=10)[0], 429)
        operation.assert_called_once_with()
        sleep.assert_not_called()


class RetryBoundsTests(unittest.TestCase):
    def test_repeated_429_stops_at_monotonic_deadline(self):
        clock = [0.0]
        operation = mock.Mock(return_value=(429, {"Retry-After": "6"}, b"limited"))
        with mock.patch.object(retry.time, "monotonic", side_effect=lambda: clock[0]), \
                mock.patch.object(retry.time, "sleep", side_effect=lambda delay: clock.__setitem__(0, clock[0] + delay)):
            self.assertEqual(retry.retry_429(operation, seconds=10)[0], 429)
        self.assertEqual(operation.call_count, 2)
        self.assertEqual(clock[0], 6.0)

    def test_malformed_or_zero_hint_has_bounded_attempts(self):
        for hint in ("not-a-number", "0", "-1"):
            operation = mock.Mock(return_value=(429, {"Retry-After": hint}, b"limited"))
            with mock.patch.object(retry.time, "sleep") as sleep:
                self.assertEqual(retry.retry_429(operation, max_attempts=3)[0], 429)
            self.assertEqual(operation.call_count, 3)
            self.assertEqual(sleep.call_args_list, [mock.call(1.0), mock.call(1.0)])


if __name__ == "__main__":
    unittest.main()
