"""Definitions in bench_report.py, pinned with numbers worked out by hand (python3 -I tests/k6/bench_report_test.py)."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import bench_report as r  # noqa: E402


def summary(ok_count, counter_rate, start_ms, create_end_ms):
    return {
        "metrics": {
            "rpc_ok{rpc:CreateOrder}": {"count": ok_count, "rate": counter_rate},
            "bench_iter_start_ms": {"min": start_ms, "max": start_ms + 500},
            "bench_create_end_ms": {"min": create_end_ms - 500, "max": create_end_ms},
        }
    }


class Throughput(unittest.TestCase):
    def test_denominator_is_the_scenario_window_not_the_test(self):
        # 74 CreateOrders in a 1s scenario after a 1s setup(): k6's Counter.rate says 34.52/s
        # (74 / ~2.14s), the throughput is 74/s
        s = summary(74, 34.52, 1_000_000, 1_001_000)
        self.assertAlmostEqual(r.throughput(s, "CreateOrder"), 74.0)

    def test_empty_window_is_zero_not_a_crash(self):
        self.assertEqual(r.throughput(summary(0, 0, 5, 5 - 1000), "CreateOrder"), 0.0)


class Drain(unittest.TestCase):
    def test_numerator_is_what_was_left_at_the_end_of_create(self):
        # 5000 orders in total, 1000 still PENDING when create ended, drained in 10s: 100/s, not 5000/10
        self.assertAlmostEqual(r.drain_rate({"total_orders": 5000, "pending_at_end": 1000, "seconds": 10}), 100.0)

    def test_nothing_left_is_not_a_rate(self):
        self.assertIsNone(r.drain_rate({"total_orders": 5000, "pending_at_end": 0, "seconds": 0.2}))


if __name__ == "__main__":
    unittest.main()
