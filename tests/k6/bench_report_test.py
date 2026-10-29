"""Definitions in bench_report.py, pinned with numbers worked out by hand (python3 -I tests/k6/bench_report_test.py)."""
import os
import sys
import tempfile
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


def reserve_summary(created, reserved):
    return {"metrics": {"rpc_ok{rpc:CreateOrder}": {"count": created}, "reserved_ok": {"count": reserved}}}


class Reconcile(unittest.TestCase):
    def test_every_created_order_must_be_seen_reserved(self):
        self.assertIsNone(r.reconcile_problem(reserve_summary(5, 5)))
        self.assertIn("1 unfinished", r.reconcile_problem(reserve_summary(5, 4)))

    def test_nothing_created_is_a_failure_not_a_100_percent(self):
        self.assertIsNotNone(r.reconcile_problem(reserve_summary(0, 0)))

    def test_reservation_success_is_reserved_over_created_not_the_createorder_success_rate(self):
        # CreateOrder succeeded every time (100%), but only 1 of 2 orders was seen RESERVED: 50%
        self.assertAlmostEqual(r.reserve_success_pct(reserve_summary(2, 1)), 50.0)

    def test_k6_exit_0_with_unfinished_iterations_is_caught_in_the_log(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "run1-reserve.log"), "w") as f:
                f.write("WARN No script iterations fully finished, consider making the test duration longer\n")
            self.assertIn("No script iterations fully finished", r.log_problem(d, 1))
            self.assertIsNone(r.log_problem(d, 2))


class Validity(unittest.TestCase):
    def test_one_sample_over_the_limit_invalidates_the_attempt(self):
        self.assertTrue(r.validity([3.0, 7.9, 8.0], 8.0)["valid"])
        v = r.validity([3.0, 8.1, 2.0], 8.0)
        self.assertFalse(v["valid"])
        self.assertEqual(v["max"], 8.1)

    def test_no_samples_is_no_proof(self):
        self.assertFalse(r.validity([], 8.0)["valid"])


def write_attempt(d, n, load, created=10, reserved=10, rate_ms=1000):
    def dump(name, obj):
        with open(os.path.join(d, f"run{n}-{name}"), "w") as f:
            f.write(obj if isinstance(obj, str) else __import__("json").dumps(obj))

    base = {
        "bench_iter_start_ms": {"min": 0, "max": 0},
        "bench_create_end_ms": {"min": 0, "max": rate_ms},
        "rpc_ok{rpc:CreateOrder}": {"count": created},
        "rpc_errors": {"count": 0},
        "reserved_ok": {"count": reserved},
        "rpc_duration{rpc:CreateOrder}": {"med": 5.0, "p(95)": 9.0, "p(99)": 12.0},
        "order_to_reserved": {"med": 150.0 + n, "p(95)": 250.0, "p(99)": 300.0},
        "reserved_after_create": {"med": 148.0, "p(95)": 240.0, "p(99)": 290.0},
    }
    dump("create.json", {"metrics": base})
    dump("reserve.json", {"metrics": base})
    dump("drain.json", {"total_orders": 100, "pending_at_end": 50, "seconds": 5})
    dump("load.csv", "\n".join(f"{i},{x}" for i, x in enumerate(load)) + "\n")


ENV = {
    "BENCH_RUNS": "2", "BENCH_NCPU": "10", "BENCH_MAX_LOAD_PER_CORE": "0.8", "BENCH_COMMIT": "c", "BENCH_DIRTY": "",
    "BENCH_GO": "g", "BENCH_PG": "p", "BENCH_K6": "k", "BENCH_CPU": "cpu", "BENCH_OS": "os", "BENCH_STAMP": "s",
    "BENCH_SKUS": "50", "BENCH_POLL": "50", "BENCH_SEED": "1", "BENCH_CREATE_VUS": "20", "BENCH_CREATE_DURATION": "10s",
    "BENCH_RESERVE_RATE": "100", "BENCH_RESERVE_DURATION": "30s",
}


class Selection(unittest.TestCase):
    def test_statistics_use_valid_attempts_only_and_invalid_ones_are_listed(self):
        with tempfile.TemporaryDirectory() as d:
            write_attempt(d, 1, [5.0, 7.0])    # valid (limit 8.0)
            write_attempt(d, 2, [5.0, 14.0])   # invalid
            write_attempt(d, 3, [6.0, 7.5])    # valid
            result, problems, code = r.report(d, ENV, 3)
        self.assertEqual(code, 0)
        self.assertEqual(problems, [])
        self.assertEqual([a["valid"] for a in result["attempts"]], [True, False, True])
        self.assertEqual([a["used_in_statistics"] for a in result["attempts"]], [True, False, True])
        # order_to_reserved med is 150+n: attempts 1 and 3 only -> median of [151, 153]; attempt 2 (152) is out
        self.assertEqual(result["reservation"]["order_to_reserved_ms"]["p50"]["runs"], [151.0, 153.0])

    def test_too_few_valid_attempts_is_not_a_baseline(self):
        with tempfile.TemporaryDirectory() as d:
            write_attempt(d, 1, [5.0, 7.0])
            write_attempt(d, 2, [5.0, 14.0])
            result, problems, code = r.report(d, ENV, 2)
        self.assertEqual(code, 3)
        self.assertFalse(result["valid"])
        self.assertIn("not a baseline", problems[0])

    def test_an_unfinished_reservation_fails_the_report_even_with_valid_load(self):
        with tempfile.TemporaryDirectory() as d:
            write_attempt(d, 1, [5.0], created=10, reserved=9)
            write_attempt(d, 2, [5.0])
            _, problems, code = r.report(d, ENV, 2)
        self.assertEqual(code, 1)
        self.assertIn("attempt 1", problems[0])


if __name__ == "__main__":
    unittest.main()
