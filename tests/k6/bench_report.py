"""Folds the runs of tests/k6/bench.sh into result.json and summary.md.

    python3 -I tests/k6/bench_report.py <out dir>

Reads run<n>-create.json / run<n>-reserve.json (k6 --summary-export) and run<n>-drain.json from
the directory; the environment (commit, Go / Postgres / k6 versions, CPU, parameters) comes from
the BENCH_* variables bench.sh sets. Definitions (also in docs/perf-baseline.md):
  throughput  successful CreateOrders / (the last CreateOrder's return - the first iteration's start),
              from bench_create_end_ms / bench_iter_start_ms: the scenario's own clock, not k6's
              Counter.rate (its denominator is the whole test, setup() included)
  drain       orders still PENDING when the create phase ended / seconds until none is
              (numerator and denominator cover the same window)
Per metric it reports the median over the runs and the
spread: min, max and the coefficient of variation (stdev / mean, population). Nothing here reads
a log or a response, only numbers k6 computed.
"""
import json
import os
import statistics
import sys

def load(out, name):
    with open(os.path.join(out, name)) as f:
        return json.load(f)


def spread(values):
    mean = statistics.fmean(values)
    return {
        "median": statistics.median(values),
        "min": min(values),
        "max": max(values),
        "cv_pct": (statistics.pstdev(values) / mean * 100) if mean else 0.0,
        "runs": values,
    }


def metric(summary, name, key):
    return summary["metrics"][name][key]


def throughput(summary, rpc):
    """rpc_ok{rpc} per second of the scenario's own window (see the module docstring)."""
    m = summary["metrics"]
    window_ms = m["bench_create_end_ms"]["max"] - m["bench_iter_start_ms"]["min"]
    return m[f"rpc_ok{{rpc:{rpc}}}"]["count"] / (window_ms / 1000) if window_ms > 0 else 0.0


def drain_rate(d):
    """orders/s the worker reserved while draining what the create phase left; None if nothing was left."""
    if d["pending_at_end"] <= 0 or d["seconds"] <= 0:
        return None
    return d["pending_at_end"] / d["seconds"]


def success_pct(summary, rpc):
    good = summary["metrics"].get(f"rpc_ok{{rpc:{rpc}}}", {}).get("count", 0)
    bad = summary["metrics"]["rpc_errors"].get("count", 0)
    return good / (good + bad) * 100 if good + bad else 0.0


def main():
    out = sys.argv[1]
    env = os.environ
    runs = int(env["BENCH_RUNS"])
    create = [load(out, f"run{n}-create.json") for n in range(1, runs + 1)]
    reserve = [load(out, f"run{n}-reserve.json") for n in range(1, runs + 1)]
    drain = [load(out, f"run{n}-drain.json") for n in range(1, runs + 1)]
    write(out, env, runs, create, reserve, drain)


def write(out, env, runs, create, reserve, drain):
    latency = lambda runs_, name: {  # noqa: E731
        q: spread([metric(r, name, key) for r in runs_])
        for q, key in (("p50", "med"), ("p95", "p(95)"), ("p99", "p(99)"))
    }

    result = {
        "environment": {
            "commit": env["BENCH_COMMIT"],
            "dirty": bool(env["BENCH_DIRTY"]),
            "go": env["BENCH_GO"],
            "postgres": env["BENCH_PG"],
            "k6": env["BENCH_K6"],
            "cpu": env["BENCH_CPU"],
            "cpus": int(env["BENCH_NCPU"]),
            "os": env["BENCH_OS"],
            "taken_at": env["BENCH_STAMP"],
        "load_avg_1m_start": float(env["BENCH_LOAD_START"]),
        "load_avg_1m_end": round(os.getloadavg()[0], 2),
        },
        "parameters": {
            "runs": runs,
            "skus": int(env["BENCH_SKUS"]),
            "poll_ms": int(env["BENCH_POLL"]),
            "seed": int(env["BENCH_SEED"]),
            "create": {"vus": int(env["BENCH_CREATE_VUS"]), "duration": env["BENCH_CREATE_DURATION"]},
            "reserve": {"rate_per_s": int(env["BENCH_RESERVE_RATE"]), "duration": env["BENCH_RESERVE_DURATION"]},
        },
        "create_order": {
            "throughput_per_s": spread([throughput(r, "CreateOrder") for r in create]),
            "success_pct": spread([success_pct(r, "CreateOrder") for r in create]),
            "latency_ms": latency(create, "rpc_duration{rpc:CreateOrder}"),
        },
        "backlog_drain": {
            "pending_at_end_of_create": spread([d["pending_at_end"] for d in drain]),
            "orders_per_s": spread([drain_rate(d) for d in drain if drain_rate(d) is not None] or [0.0]),
            "seconds": spread([d["seconds"] for d in drain]),
        },
        "reservation": {
            "orders_per_s": spread([throughput(r, "CreateOrder") for r in reserve]),
            "success_pct": spread([success_pct(r, "CreateOrder") for r in reserve]),
            "dropped_iterations": spread([r["metrics"].get("dropped_iterations", {}).get("count", 0) for r in reserve]),
            "order_to_reserved_ms": latency(reserve, "order_to_reserved"),
            "reserved_after_create_ms": latency(reserve, "reserved_after_create"),
        },
    }

    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(result, f, indent=2)
        f.write("\n")


    def row(label, s, unit="", digits=1):
        f = f"{{:.{digits}f}}"
        return (
            f"| {label} | {f.format(s['median'])}{unit} | {f.format(s['min'])}–{f.format(s['max'])}{unit} "
            f"| {s['cv_pct']:.1f}% |"
        )


    e, p = result["environment"], result["parameters"]
    lines = [
        f"# k6 bench {e['taken_at']}",
        "",
        f"- commit `{e['commit']}`{' (dirty)' if e['dirty'] else ''}",
        f"- {e['go']}; Postgres {e['postgres']}; {e['k6']}",
        f"- {e['cpu']} ({e['cpus']} cores), {e['os']}; load average (1m) {e['load_avg_1m_start']} at start, {e['load_avg_1m_end']} at end",
        f"- {p['runs']} runs; create: {p['create']['vus']} VUs × {p['create']['duration']}; "
        f"reserve: {p['reserve']['rate_per_s']} orders/s × {p['reserve']['duration']}; "
        f"{p['skus']} SKUs, poll {p['poll_ms']}ms, seed {p['seed']}",
        "",
        "Median over the runs, min–max and coefficient of variation (stdev/mean).",
        "",
        "| metric | median | min–max | CV |",
        "|---|---|---|---|",
    ]
    c, d, r = result["create_order"], result["backlog_drain"], result["reservation"]
    lines += [
        row("CreateOrder throughput (req/s)", c["throughput_per_s"], digits=0),
        row("CreateOrder success", c["success_pct"], "%", 2),
        row("CreateOrder p50 (ms)", c["latency_ms"]["p50"], digits=2),
        row("CreateOrder p95 (ms)", c["latency_ms"]["p95"], digits=2),
        row("CreateOrder p99 (ms)", c["latency_ms"]["p99"], digits=2),
        row("PENDING when create ended (orders)", d["pending_at_end_of_create"], digits=0),
        row("backlog drain (orders/s)", d["orders_per_s"], digits=0),
        row("backlog drain (s)", d["seconds"], digits=1),
        row("reserve: orders created (req/s)", r["orders_per_s"], digits=1),
        row("reserve: success", r["success_pct"], "%", 2),
        row("reserve: dropped iterations", r["dropped_iterations"], digits=0),
        row("order → RESERVED p50 (ms, from before CreateOrder)", r["order_to_reserved_ms"]["p50"], digits=0),
        row("order → RESERVED p95 (ms, from before CreateOrder)", r["order_to_reserved_ms"]["p95"], digits=0),
        row("order → RESERVED p99 (ms, from before CreateOrder)", r["order_to_reserved_ms"]["p99"], digits=0),
        row("CreateOrder returned → RESERVED p50 (ms)", r["reserved_after_create_ms"]["p50"], digits=0),
        row("CreateOrder returned → RESERVED p95 (ms)", r["reserved_after_create_ms"]["p95"], digits=0),
        row("CreateOrder returned → RESERVED p99 (ms)", r["reserved_after_create_ms"]["p99"], digits=0),
    ]
    with open(os.path.join(out, "summary.md"), "w") as f:
        f.write("\n".join(lines) + "\n")



if __name__ == "__main__":
    main()
