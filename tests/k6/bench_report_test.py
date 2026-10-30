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
    def status(self, before, during=(), marker=None, limit=8.0):
        with tempfile.TemporaryDirectory() as d:
            for kind, xs in (("baseline", before), ("load", during)):
                if xs is not None:
                    with open(os.path.join(d, f"run1-{kind}.csv"), "w") as f:
                        f.write("".join(f"{i},{x}\n" for i, x in enumerate(xs)))
            if marker:
                with open(os.path.join(d, "run1-invalid.txt"), "w") as f:
                    f.write(marker)
            return r.attempt_status(d, 1, limit)

    def test_the_last_sample_before_the_attempt_is_judged_against_the_limit(self):
        self.assertTrue(self.status([9.0, 8.0])["valid"])
        v = self.status([3.0, 8.1])
        self.assertFalse(v["valid"])
        self.assertIn("not quiet", v["reason"])
        self.assertEqual(v["baseline"], 8.1)

    def test_load_while_the_attempt_ran_is_recorded_not_judged(self):
        v = self.status([1.0], [3.0, 50.0])
        self.assertTrue(v["valid"])
        self.assertEqual((v["max"], v["mean"], v["samples"]), (50.0, 26.5, 2))

    def test_no_sample_is_no_proof(self):
        self.assertFalse(self.status([])["valid"])
        self.assertFalse(self.status(None)["valid"])

    def test_an_attempt_bench_sh_cut_short_is_invalid_with_its_reason(self):
        v = self.status([1.0], [1.0], marker="k6 create: thresholds on metrics 'rpc_errors' have been crossed\n")
        self.assertFalse(v["valid"])
        self.assertIn("rpc_errors", v["reason"])


def write_attempt(d, n, load, created=10, reserved=10, rate_ms=1000, baseline=(0.1,)):
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
    if baseline is not None:
        dump("baseline.csv", "\n".join(f"{i},{x}" for i, x in enumerate(baseline)) + "\n")


ENV = {
    "BENCH_RUNS": "2", "BENCH_NCPU": "10", "BENCH_MAX_LOAD_PER_CORE": "0.8", "BENCH_COMMIT": "c", "BENCH_DIRTY": "",
    "BENCH_GO": "g", "BENCH_PG": "p", "BENCH_K6": "k", "BENCH_CPU": "cpu", "BENCH_OS": "os", "BENCH_STAMP": "s",
    "BENCH_SKUS": "50", "BENCH_POLL": "50", "BENCH_SEED": "1", "BENCH_CREATE_VUS": "20", "BENCH_CREATE_DURATION": "10s",
    "BENCH_RESERVE_RATE": "100", "BENCH_RESERVE_DURATION": "30s",
}


class Selection(unittest.TestCase):
    def test_statistics_use_valid_attempts_only_and_invalid_ones_are_listed(self):
        with tempfile.TemporaryDirectory() as d:
            write_attempt(d, 1, [5.0, 7.0], baseline=[2.0])    # valid (limit 8.0)
            write_attempt(d, 2, [5.0, 7.0], baseline=[14.0])   # started on a busy machine: invalid
            write_attempt(d, 3, [6.0, 50.0], baseline=[7.5])   # the bench's own load is not judged: valid
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
            write_attempt(d, 2, [5.0, 7.0], baseline=[14.0])
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


FIXTURE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "testdata", "run41000071677")


def fixture_samples(name):
    with open(os.path.join(FIXTURE, name)) as f:
        return [float(line.split(",")[1]) for line in f if line.strip()]


class EnvironmentLoadNotOwnLoad(unittest.TestCase):
    """#288: the bench's rpc / worker / Postgres / k6 are on the machine whose load is measured, so the
    load while an attempt runs is the bench's own. Only the load BEFORE the attempt says whether the
    environment was clean. Fixtures: the real samples of run 41000071677 (4 cores, limit 4.0)."""

    ENV4 = {**ENV, "BENCH_RUNS": "1", "BENCH_NCPU": "4", "BENCH_MAX_LOAD_PER_CORE": "1.0"}

    def test_a_clean_start_is_valid_whatever_the_bench_did_to_the_load(self):
        baseline = fixture_samples("baseline-at-start.csv")
        for name in ("run1-load.csv", "run2-load.csv", "run6-load.csv"):
            with self.subTest(name), tempfile.TemporaryDirectory() as d:
                load = fixture_samples(name)
                write_attempt(d, 1, load, baseline=baseline)
                result, problems, code = r.report(d, self.ENV4, 1)
            self.assertTrue(result["attempts"][0]["valid"], f"{name}: peak {max(load)} while the bench ran, {baseline[-1]} before it: the attempt is valid")
            self.assertEqual((problems, code), ([], 0))
            self.assertEqual(result["attempts"][0]["max"], max(load), "the load while the bench ran is still recorded")

    def test_a_busy_start_is_invalid_even_if_the_bench_itself_was_light(self):
        with tempfile.TemporaryDirectory() as d:
            write_attempt(d, 1, [0.5, 0.6], baseline=[6.0])
            result, _, code = r.report(d, self.ENV4, 1)
        self.assertFalse(result["attempts"][0]["valid"], "baseline load 6.0 > limit 4.0")
        self.assertEqual(code, 3)

    def test_no_baseline_sample_is_no_proof(self):
        with tempfile.TemporaryDirectory() as d:
            write_attempt(d, 1, [0.5, 0.6], baseline=None)
            result, _, _ = r.report(d, self.ENV4, 1)
        self.assertFalse(result["attempts"][0]["valid"], "no load was measured before the attempt")

    def test_an_attempt_that_overloaded_is_invalid_not_a_report_crash(self):
        env = {**self.ENV4, "BENCH_RUNS": "2"}
        with tempfile.TemporaryDirectory() as d:
            write_attempt(d, 1, [9.0], baseline=[1.7])
            for gone in ("reserve.json", "drain.json"):  # k6 crossed a threshold in the create phase: nothing after it ran
                os.remove(os.path.join(d, f"run1-{gone}"))
            with open(os.path.join(d, "run1-invalid.txt"), "w") as f:
                f.write("k6 create: thresholds on metrics 'rpc_errors' have been crossed\n")
            write_attempt(d, 2, [0.5], baseline=[1.0])
            write_attempt(d, 3, [0.5], baseline=[1.0])
            try:
                result, problems, code = r.report(d, env, 3)
            except Exception as e:  # noqa: BLE001
                self.fail(f"report raised {e!r}: an attempt cut short by overload has no k6 reserve / drain output")
        self.assertEqual([a["valid"] for a in result["attempts"]], [False, True, True])
        self.assertIn("rpc_errors", result["attempts"][0]["reason"])
        self.assertEqual((problems, code), ([], 0), "overload is a measurement that does not count, not a tool failure")

    def test_when_every_attempt_was_cut_short_it_is_not_a_baseline_exit_3(self):
        with tempfile.TemporaryDirectory() as d:
            for n in (1, 2):
                write_attempt(d, n, [9.0], baseline=[1.7])
                for gone in ("create.json", "reserve.json", "drain.json"):
                    os.remove(os.path.join(d, f"run{n}-{gone}"))
                with open(os.path.join(d, f"run{n}-invalid.txt"), "w") as f:
                    f.write("environment not quiet\n")
            try:
                _, problems, code = r.report(d, {**self.ENV4, "BENCH_RUNS": "2"}, 2)
            except Exception as e:  # noqa: BLE001
                self.fail(f"report raised {e!r}: with no k6 output at all it must still say NOT A VALID BASELINE")
        self.assertEqual(code, 3)
        self.assertIn("not a baseline", problems[-1])


class ThresholdClassification(unittest.TestCase):
    """k6 exit 99: only provable overload is an invalid attempt; everything else is a failure (fail-closed)."""

    def classify(self, metrics):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "s.json")
            if metrics is not None:
                with open(path, "w") as f:
                    f.write(metrics if isinstance(metrics, str) else __import__("json").dumps({"metrics": metrics}))
            return r.classify_threshold_failure(path, "create")

    @staticmethod
    def crossed(count, **extra):
        return {"count": count, "thresholds": {"x": True}, **extra}

    TIMEOUTS = {
        "rpc_errors": {"count": 7, "thresholds": {"count==0": True}},
        "rpc_errors{code:request.timeout}": {"count": 4, "thresholds": {"count>=0": False}},
        "rpc_errors{code:transport.timeout}": {"count": 3, "thresholds": {"count>=0": False}},
        "rpc_ok{rpc:CreateOrder}": {"count": 9, "thresholds": {"count>0": False}},
    }

    def test_rpc_errors_that_are_all_timeouts_are_overload(self):
        kind, why = self.classify(self.TIMEOUTS)
        self.assertEqual(kind, "overload", why)
        self.assertIn("rpc_errors", why)

    def test_the_real_run_41000071677_attempt_6_is_overload(self):
        # create phase of attempt 6 (20 x request.timeout): the sub-metrics did not exist yet, so build them from the log's 20 errors
        with open(os.path.join(FIXTURE, "run6-create.json")) as f:
            m = __import__("json").load(f)["metrics"]
        self.assertTrue(m["rpc_errors"]["thresholds"]["count==0"], "k6 marks a crossed threshold True")
        m["rpc_errors{code:request.timeout}"] = {"count": m["rpc_errors"]["count"], "thresholds": {"count>=0": False}}
        m["rpc_errors{code:transport.timeout}"] = {"count": 0, "thresholds": {"count>=0": False}}
        self.assertEqual(self.classify(m)[0], "overload")

    def test_reservation_timeouts_are_overload(self):
        self.assertEqual(self.classify({"reservation_timeouts": self.crossed(2)})[0], "overload")

    def test_a_correctness_threshold_is_a_failure(self):
        kind, why = self.classify({"reservation_wrong_status": self.crossed(1)})
        self.assertEqual(kind, "failure")
        self.assertIn("reservation_wrong_status", why)

    def test_correctness_wins_over_an_overload_threshold_crossed_at_the_same_time(self):
        kind, why = self.classify({**self.TIMEOUTS, "reservation_wrong_status": self.crossed(1)})
        self.assertEqual(kind, "failure")
        self.assertIn("reservation_wrong_status", why)

    def test_rpc_errors_that_are_not_all_timeouts_are_a_failure(self):
        m = {**self.TIMEOUTS, "rpc_errors{code:transport.timeout}": {"count": 0, "thresholds": {"count>=0": False}}}
        kind, why = self.classify(m)
        self.assertEqual(kind, "failure")
        self.assertIn("3 of 7 are not timeouts", why)

    def test_one_missing_per_code_metric_is_enough_to_fail_closed(self):
        # request.timeout alone adds up to rpc_errors, but a missing transport.timeout metric means the split is unknown
        m = {k: v for k, v in self.TIMEOUTS.items() if k != "rpc_errors{code:transport.timeout}"}
        m["rpc_errors{code:request.timeout}"] = {"count": 7, "thresholds": {"count>=0": False}}
        kind, why = self.classify(m)
        self.assertEqual(kind, "failure")
        self.assertIn("transport.timeout", why)

    def test_a_per_code_count_must_be_a_non_negative_integer(self):
        for label, bad in (("empty", {}), ("negative", {"count": -2}), ("string", {"count": "0"}), ("float", {"count": 0.0}), ("bool", {"count": False})):
            with self.subTest(label):
                m = {**self.TIMEOUTS, "rpc_errors{code:request.timeout}": {"count": 9 if label == "negative" else 7}, "rpc_errors{code:transport.timeout}": bad}
                self.assertEqual(self.classify(m)[0], "failure")

    def test_rpc_errors_itself_without_a_valid_count_is_a_failure(self):
        m = {**self.TIMEOUTS, "rpc_errors": {"thresholds": {"count==0": True}}}
        kind, why = self.classify(m)
        self.assertEqual(kind, "failure")
        self.assertIn("rpc_errors", why)

    def test_without_the_per_code_metrics_rpc_errors_cannot_be_called_overload(self):
        kind, _ = self.classify({"rpc_errors": {"count": 7, "thresholds": {"count==0": True}}})
        self.assertEqual(kind, "failure")

    def test_http_req_failed_alone_or_an_unknown_threshold_is_a_failure(self):
        self.assertEqual(self.classify({"http_req_failed": self.crossed(1)})[0], "failure")
        self.assertEqual(self.classify({"something_new": self.crossed(1)})[0], "failure")
        self.assertEqual(self.classify({**self.TIMEOUTS, "http_req_failed": self.crossed(1)})[0], "overload")

    def test_no_crossed_threshold_missing_or_broken_summary_is_a_failure(self):
        self.assertEqual(self.classify({"rpc_ok{rpc:CreateOrder}": {"count": 1, "thresholds": {"count>0": False}}})[0], "failure")
        self.assertEqual(self.classify(None)[0], "failure")
        self.assertEqual(self.classify("not json")[0], "failure")
        self.assertEqual(self.classify('{"metrics": []}')[0], "failure")


if __name__ == "__main__":
    unittest.main()
