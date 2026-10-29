"""Folds the attempts of tests/k6/bench.sh into result.json and summary.md.

    python3 -I tests/k6/bench_report.py <out dir>

Reads run<n>-create.json / run<n>-reserve.json (k6 --summary-export), run<n>-drain.json and
run<n>-load.csv (epoch,load1 every few seconds while attempt n ran) from the directory; the
environment (commit, Go / Postgres / k6 versions, CPU, parameters) comes from the BENCH_* variables
bench.sh sets. Definitions (also in docs/perf-baseline.md):
  throughput  successful CreateOrders / (the last CreateOrder's return - the first iteration's start),
              from bench_iter_start_ms / bench_create_end_ms: the scenario's own clock, not k6's
              Counter.rate (its denominator is the whole test, setup() included)
  drain       orders still PENDING when the create phase ended / seconds until none is
              (numerator and denominator cover the same window)
  valid       an attempt is valid iff every load-average sample taken while it ran is
              <= MAX_LOAD_PER_CORE x cores (fixed before the run, judged here, not by a person who has
              seen the numbers). Statistics use the valid attempts only; invalid ones are listed, never
              dropped from the directory. Fewer valid attempts than RUNS = not a baseline (exit 3).
  reconcile   per attempt, CreateOrders that succeeded == reservations seen RESERVED. An order that was
              created but whose wait was cut off (k6's gracefulStop) is neither a success nor a
              timeout; k6 exits 0 and says only "No script iterations fully finished" — also a failure.
Per metric: median over the attempts, min, max and the coefficient of variation (stdev / mean,
population). Nothing here reads a response, only numbers k6 and bench.sh computed.
Exit: 0 ok, 1 reconcile / log failure, 3 not enough valid attempts.
"""
import json
import os
import statistics
import sys

INTERRUPTED = "No script iterations fully finished"
NOT_A_BASELINE = "not a baseline"


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


def created_and_reserved(summary):
    m = summary["metrics"]
    return m["rpc_ok{rpc:CreateOrder}"]["count"], m.get("reserved_ok", {}).get("count", 0)


def reserve_success_pct(summary):
    """Orders that were created and then seen RESERVED, of the orders that were created."""
    created, reserved = created_and_reserved(summary)
    return reserved / created * 100 if created else 0.0


def reconcile_problem(summary):
    """None if every created order was seen RESERVED, else what does not add up."""
    created, reserved = created_and_reserved(summary)
    if created == 0:
        return "no CreateOrder succeeded"
    if created != reserved:
        return f"{created} CreateOrder succeeded but {reserved} were seen RESERVED ({created - reserved} unfinished)"
    return None


def load_samples(out, n):
    try:
        with open(os.path.join(out, f"run{n}-load.csv")) as f:
            return [float(line.split(",")[1]) for line in f if line.strip()]
    except FileNotFoundError:
        return []


def validity(samples, limit):
    """valid iff there are samples and none exceeds the limit (no samples = no proof)."""
    top = max(samples) if samples else None
    return {
        "samples": len(samples),
        "max": top,
        "mean": round(statistics.fmean(samples), 2) if samples else None,
        "limit": limit,
        "valid": bool(samples) and top <= limit,
    }


def log_problem(out, n):
    for phase in ("create", "reserve"):
        try:
            with open(os.path.join(out, f"run{n}-{phase}.log"), errors="replace") as f:
                if INTERRUPTED in f.read():
                    return f"k6 said '{INTERRUPTED}' in run{n}-{phase}.log"
        except FileNotFoundError:
            pass
    return None


def main():
    out = sys.argv[1]
    env = os.environ
    result, problems, code = report(out, env, int(env["BENCH_ATTEMPTS"]))
    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(result, f, indent=2)
        f.write("\n")
    with open(os.path.join(out, "summary.md"), "w") as f:
        f.write(summary_md(result, problems))
    for p in problems:
        print(f"FAIL: {p}", file=sys.stderr)
    sys.exit(code)


def report(out, env, attempts):
    runs = int(env["BENCH_RUNS"])
    cpus = int(env["BENCH_NCPU"])
    limit = float(env["BENCH_MAX_LOAD_PER_CORE"]) * cpus
    idx = list(range(1, attempts + 1))
    create = {n: load(out, f"run{n}-create.json") for n in idx}
    reserve = {n: load(out, f"run{n}-reserve.json") for n in idx}
    drain = {n: load(out, f"run{n}-drain.json") for n in idx}
    valid_of = {n: validity(load_samples(out, n), limit) for n in idx}

    problems = []
    for n in idx:
        p = reconcile_problem(reserve[n]) or log_problem(out, n)
        if p:
            problems.append(f"attempt {n}: {p}")
    failed = bool(problems)

    valid = [n for n in idx if valid_of[n]["valid"]]
    used = valid or idx  # nothing valid: the numbers are still shown, under a banner saying they are not a baseline
    if len(valid) < runs:
        problems.append(f"{len(valid)} valid attempt(s) of the {runs} required: {NOT_A_BASELINE}")

    pick = lambda d: [d[n] for n in used]  # noqa: E731
    latency = lambda ds, name: {  # noqa: E731
        q: spread([metric(r, name, key) for r in ds])
        for q, key in (("p50", "med"), ("p95", "p(95)"), ("p99", "p(99)"))
    }
    c_, r_, d_ = pick(create), pick(reserve), pick(drain)

    result = {
        "valid": not problems,
        "validity_rule": f"every load-average (1m) sample while an attempt ran <= {env['BENCH_MAX_LOAD_PER_CORE']} per core x {cpus} cores = {limit}",
        "attempts": [{"attempt": n, **valid_of[n], "used_in_statistics": n in used} for n in idx],
        "environment": {
            "commit": env["BENCH_COMMIT"],
            "dirty": bool(env["BENCH_DIRTY"]),
            "go": env["BENCH_GO"],
            "postgres": env["BENCH_PG"],
            "k6": env["BENCH_K6"],
            "cpu": env["BENCH_CPU"],
            "cpus": cpus,
            "os": env["BENCH_OS"],
            "taken_at": env["BENCH_STAMP"],
        },
        "parameters": {
            "runs": runs,
            "attempts": attempts,
            "max_load_per_core": float(env["BENCH_MAX_LOAD_PER_CORE"]),
            "skus": int(env["BENCH_SKUS"]),
            "poll_ms": int(env["BENCH_POLL"]),
            "seed": int(env["BENCH_SEED"]),
            "create": {"vus": int(env["BENCH_CREATE_VUS"]), "duration": env["BENCH_CREATE_DURATION"]},
            "reserve": {"rate_per_s": int(env["BENCH_RESERVE_RATE"]), "duration": env["BENCH_RESERVE_DURATION"]},
        },
        "create_order": {
            "throughput_per_s": spread([throughput(r, "CreateOrder") for r in c_]),
            "success_pct": spread([success_pct(r, "CreateOrder") for r in c_]),
            "latency_ms": latency(c_, "rpc_duration{rpc:CreateOrder}"),
        },
        "backlog_drain": {
            "pending_at_end_of_create": spread([d["pending_at_end"] for d in d_]),
            "orders_per_s": spread([drain_rate(d) for d in d_ if drain_rate(d) is not None] or [0.0]),
            "seconds": spread([d["seconds"] for d in d_]),
        },
        "reservation": {
            "orders_per_s": spread([throughput(r, "CreateOrder") for r in r_]),
            "success_pct": spread([reserve_success_pct(r) for r in r_]),
            "dropped_iterations": spread([r["metrics"].get("dropped_iterations", {}).get("count", 0) for r in r_]),
            "order_to_reserved_ms": latency(r_, "order_to_reserved"),
            "reserved_after_create_ms": latency(r_, "reserved_after_create"),
        },
    }
    return result, problems, (1 if failed else (3 if problems else 0))


def row(label, s, unit="", digits=1):
    f = f"{{:.{digits}f}}"
    return f"| {label} | {f.format(s['median'])}{unit} | {f.format(s['min'])}–{f.format(s['max'])}{unit} | {s['cv_pct']:.1f}% |"


def summary_md(result, problems):
    e, p = result["environment"], result["parameters"]
    lines = [f"# k6 bench {e['taken_at']}", ""]
    if problems:
        lines += ["> **NOT A VALID BASELINE**", *[f"> - {x}" for x in problems], ""]
    lines += [
        f"- commit `{e['commit']}`{' (dirty)' if e['dirty'] else ''}",
        f"- {e['go']}; Postgres {e['postgres']}; {e['k6']}",
        f"- {e['cpu']} ({e['cpus']} cores), {e['os']}",
        f"- validity rule: {result['validity_rule']}",
        f"- {p['runs']} valid runs wanted, {p['attempts']} attempted; create: {p['create']['vus']} VUs × {p['create']['duration']}; "
        f"reserve: {p['reserve']['rate_per_s']} orders/s × {p['reserve']['duration']}; "
        f"{p['skus']} SKUs, poll {p['poll_ms']}ms, seed {p['seed']}",
        "",
        "| attempt | load samples | max | mean | valid | in statistics |",
        "|---|---|---|---|---|---|",
    ]
    for a in result["attempts"]:
        lines.append(
            f"| {a['attempt']} | {a['samples']} | {a['max']} | {a['mean']} | {'yes' if a['valid'] else '**NO**'} "
            f"| {'yes' if a['used_in_statistics'] else 'no'} |"
        )
    lines += [
        "",
        "Median over the attempts in statistics, min–max and coefficient of variation (stdev/mean).",
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
        row("reserve: created orders seen RESERVED", r["success_pct"], "%", 2),
        row("reserve: dropped iterations (never started)", r["dropped_iterations"], digits=0),
        row("order → RESERVED p50 (ms, from before CreateOrder)", r["order_to_reserved_ms"]["p50"], digits=0),
        row("order → RESERVED p95 (ms, from before CreateOrder)", r["order_to_reserved_ms"]["p95"], digits=0),
        row("order → RESERVED p99 (ms, from before CreateOrder)", r["order_to_reserved_ms"]["p99"], digits=0),
        row("CreateOrder returned → RESERVED p50 (ms)", r["reserved_after_create_ms"]["p50"], digits=0),
        row("CreateOrder returned → RESERVED p95 (ms)", r["reserved_after_create_ms"]["p95"], digits=0),
        row("CreateOrder returned → RESERVED p99 (ms)", r["reserved_after_create_ms"]["p99"], digits=0),
    ]
    return "\n".join(lines) + "\n"


if __name__ == "__main__":
    main()
