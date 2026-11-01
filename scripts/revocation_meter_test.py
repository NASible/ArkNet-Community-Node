import unittest

from scripts import revocation_meter as meter

KIB = 1024


def steady(start, end, rate, step=0.5, t0=0.0, rx0=0):
    """Samples from t0+start to t0+end growing at rate bytes/s."""
    samples, t, rx = [], start, rx0
    while t <= end + 1e-9:
        samples.append((t0 + t, rx))
        t += step
        rx += int(rate * step)
    return samples


class AnalyzeTests(unittest.TestCase):
    def test_counters_that_stop_inside_the_bound_pass(self):
        active = steady(0, 13, 512 * KIB)  # revoked at t=5, stops at t=13
        last = active[-1][1]
        quiet = [(13 + 0.5 * i, last + 100 * i) for i in range(1, 25)]  # small control traffic
        result = meter.analyze(active + quiet, revoked_at=5, bound=15)
        self.assertAlmostEqual(result["stopped_after"], 8, delta=0.5)
        self.assertGreater(result["rate_before"], 256 * KIB)
        self.assertLess(result["residual_bytes"], 64 * KIB)

    def test_counters_that_keep_running_fail(self):
        samples = steady(0, 40, 512 * KIB)
        with self.assertRaisesRegex(AssertionError, "did not stop"):
            meter.analyze(samples, revoked_at=5, bound=15)

    def test_stopping_after_the_bound_fails(self):
        active = steady(0, 25, 512 * KIB)
        quiet = [(25 + 0.5 * i, active[-1][1]) for i in range(1, 25)]
        with self.assertRaisesRegex(AssertionError, "did not stop"):
            meter.analyze(active + quiet, revoked_at=5, bound=15)

    def test_an_idle_transfer_proves_nothing(self):
        samples = [(0.5 * i, 1000) for i in range(80)]
        with self.assertRaisesRegex(AssertionError, "not receiving"):
            meter.analyze(samples, revoked_at=5, bound=15)

    def test_too_short_a_quiet_window_proves_nothing(self):
        active = steady(0, 13, 512 * KIB)
        quiet = [(13 + 0.5 * i, active[-1][1]) for i in range(1, 6)]  # 2.5 s
        with self.assertRaisesRegex(AssertionError, "quiet window"):
            meter.analyze(active + quiet, revoked_at=5, bound=15)

    def test_many_small_increments_after_the_stop_count_as_residual(self):
        active = steady(0, 13, 512 * KIB)
        quiet = [(13 + 0.5 * i, active[-1][1] + 15 * KIB * i) for i in range(1, 25)]  # each < one block
        with self.assertRaisesRegex(AssertionError, "residual"):
            meter.analyze(active + quiet, revoked_at=5, bound=15)


    def test_quiet_window_can_be_required_longer_than_a_source_retry(self):
        # A source that pauses (e.g. a web-seed retry wait) and resumes must fail.
        active = steady(0, 8, 512 * KIB)
        pause = [(8 + 0.5 * i, active[-1][1]) for i in range(1, 50)]       # 24.5 s silent
        resumed = steady(32.5, 40, 512 * KIB, rx0=active[-1][1])
        with self.assertRaisesRegex(AssertionError, "did not stop"):
            meter.analyze(active + pause + resumed, revoked_at=5, bound=15, quiet_seconds=30)
        with self.assertRaisesRegex(AssertionError, "quiet window"):
            meter.analyze(active + pause, revoked_at=5, bound=15, quiet_seconds=30)


class RecentRateTests(unittest.TestCase):
    def test_recent_rate_uses_the_last_seconds_only(self):
        samples = steady(0, 10, 512 * KIB) + [(10 + 0.5 * i, steady(0, 10, 512 * KIB)[-1][1]) for i in range(1, 7)]
        self.assertEqual(meter.recent_rate(samples, seconds=2), 0)
        self.assertGreater(meter.recent_rate(samples[:21], seconds=2), 400 * KIB)

    def test_recent_rate_needs_two_samples(self):
        self.assertEqual(meter.recent_rate([(0, 5)]), 0)


class ParseTests(unittest.TestCase):
    def test_announce_counts(self):
        body = b"d8:completei1e10:incompletei2e8:intervali1800e5:peers0:6:peers60:e"
        self.assertEqual(meter.announce_counts(body), (1, 2))

    def test_failure_is_an_error(self):
        with self.assertRaises(AssertionError):
            meter.announce_counts(b"d14:failure reason13:invalid granted")


if __name__ == "__main__":
    unittest.main()
