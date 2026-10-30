#!/usr/bin/env bash
# bench.sh against fake k6 / psql / binaries (no Postgres, no rpc): what it decides when the machine is
# busy before an attempt, when the bench's own load is high, and when k6 fails (#288). `task k6-bench-test`;
# not part of CI. The machine's load is the number in $BENCH_LOADAVG_FILE (tests/k6/loadavg.py); the
# fake k6 raises it while it "runs" and lowers it when it ends, as the real bench does to a real machine.
set -euo pipefail
work=$(mktemp -d)
bg=""
trap 'for p in $bg; do kill "$p" 2>/dev/null || true; done; rm -rf "$work"' EXIT

mkdir "$work/fakebin" "$work/bin"
cat > "$work/fakebin/psql" <<'EOF'
#!/bin/sh
sql=""
while [ $# -gt 0 ]; do [ "$1" = -c ] && sql=$2; shift; done
case "$sql" in
  "show server_version") echo 16.0 ;;
  "select count(*) filter"*) echo "0|10" ;;
  "select count(*) from ordering.orders where status = 'PENDING'") echo 0 ;;
  "select count(*) from ordering.orders where status <> 'CONFIRMED'") echo 0 ;;
  "select count(*) from inventory.reservations"*) echo 10 ;;
esac
EOF
printf '#!/bin/sh\nexit 0\n' > "$work/fakebin/curl"
printf '#!/bin/sh\nexit 0\n' > "$work/bin/migrate"
printf '#!/bin/sh\nexec sleep 600\n' > "$work/bin/rpc"
cp "$work/bin/rpc" "$work/bin/worker"
# FAKE_K6=ok|script|<crossing>. A run raises the machine's load to FAKE_K6_BUSY for 2s (the create phase) and
# puts it back to FAKE_K6_QUIET when it ends; script = exit 107; a crossing is k6's exit 99 in the create phase,
# with the --summary-export k6 writes for it (a crossed threshold is `"thresholds": {expr: true}`):
#   timeouts   rpc_errors crossed, every error is request.timeout (what an overloaded machine produces)
#   wrong      reservation_wrong_status crossed (a correctness threshold)
#   both       rpc_errors (all timeouts) and reservation_wrong_status crossed
#   other      rpc_errors crossed, 4 of the 7 errors are not timeouts
#   nosummary  exit 99 and no summary file at all
#   emptycount like timeouts, but rpc_errors{code:transport.timeout} is {} (no count)
#   negative   like timeouts, but request.timeout is 9 and transport.timeout -2 (they add up to the 7 rpc_errors)
#   rpcnocount like timeouts, but rpc_errors itself has no count
#   okless     k6 exits 0 and writes no summary
#   onlyreq    like timeouts, but the summary has no rpc_errors{code:transport.timeout} sub-metric
cat > "$work/fakebin/k6" <<'EOF'
#!/usr/bin/env python3
import json, os, sys, time
a = sys.argv[1:]
if a[:1] == ["version"]:
    print("k6 v0.0.0 (fake)"); sys.exit(0)
out = a[a.index("--summary-export") + 1]
phase = next(x.split("=")[1] for x in a if x.startswith("PHASE="))
mode, load = os.environ["FAKE_K6"], os.environ["BENCH_LOADAVG_FILE"]
with open(os.environ["FAKE_K6_LOG"], "a") as f:
    f.write(phase + "\n")
def setload(v):
    with open(load, "w") as f:
        f.write(v)
m = {
    "bench_iter_start_ms": {"min": 0, "max": 0}, "bench_create_end_ms": {"min": 0, "max": 1000},
    "rpc_ok{rpc:CreateOrder}": {"count": 10}, "rpc_errors": {"count": 0}, "reserved_ok": {"count": 10},
    "rpc_duration{rpc:CreateOrder}": {"med": 5.0, "p(95)": 9.0, "p(99)": 12.0},
    "order_to_reserved": {"med": 150.0, "p(95)": 250.0, "p(99)": 300.0},
    "reserved_after_create": {"med": 148.0, "p(95)": 240.0, "p(99)": 290.0},
}
if phase == "create":
    setload(os.environ["FAKE_K6_BUSY"]); time.sleep(2)
if mode == "okless":
    setload(os.environ["FAKE_K6_QUIET"]); sys.exit(0)
if mode == "script":
    setload(os.environ["FAKE_K6_QUIET"]); print("GoError: fake script error"); sys.exit(107)
if mode in ("timeouts", "wrong", "both", "other", "nosummary", "onlyreq", "emptycount", "negative", "rpcnocount"):
    if mode in ("timeouts", "both", "other", "onlyreq", "emptycount", "negative", "rpcnocount"):
        m["rpc_errors"] = {"count": 7, "thresholds": {"count==0": True}}
        m["rpc_errors{code:request.timeout}"] = {"count": 3 if mode == "other" else 7, "thresholds": {"count>=0": False}}
        if mode != "onlyreq":
            m["rpc_errors{code:transport.timeout}"] = {"count": 0, "thresholds": {"count>=0": False}}
        if mode == "emptycount":
            m["rpc_errors{code:transport.timeout}"] = {}
        if mode == "negative":
            m["rpc_errors{code:request.timeout}"]["count"] = 9
            m["rpc_errors{code:transport.timeout}"]["count"] = -2
        if mode == "rpcnocount":
            m["rpc_errors"] = {"thresholds": {"count==0": True}}
    if mode in ("wrong", "both"):
        m["reservation_wrong_status"] = {"count": 2, "thresholds": {"count==0": True}}
    if mode != "nosummary":
        json.dump({"metrics": m}, open(out, "w"))
    setload(os.environ["FAKE_K6_QUIET"])
    print("time=\"2026-10-30T04:02:49Z\" level=error msg=\"thresholds on metrics 'rpc_errors' have been crossed\"", file=sys.stderr)
    sys.exit(99)
json.dump({"metrics": m}, open(out, "w"))
if phase == "reserve":
    setload(os.environ["FAKE_K6_QUIET"])
EOF
chmod +x "$work/fakebin/"* "$work/bin/"*

fails=0
check() { if [ "$1" = ok ]; then echo "--- PASS: $2"; else echo "--- FAIL: $2"; fails=$((fails + 1)); fi; }

# bench <name> <initial load> <FAKE_K6 mode> [extra env…]: 3 attempts wanted of at most 3; $work/<name>.{rc,log,k6,out}
bench() {
  local name=$1 initial=$2 mode=$3; shift 3
  echo "$initial" > "$work/load"; : > "$work/$name.k6"
  local rc=0
  env PATH="$work/fakebin:$PATH" BENCH_LOADAVG_FILE="$work/load" LOAD_INTERVAL=1 RUNS=3 MAX_ATTEMPTS=3 \
    FAKE_K6="$mode" FAKE_K6_LOG="$work/$name.k6" FAKE_K6_BUSY=1000 FAKE_K6_QUIET=0.5 "$@" \
    tests/k6/bench.sh "$work/bin" "$work/$name.out" > "$work/$name.log" 2>&1 || rc=$?
  echo "$rc" > "$work/$name.rc"
}
rc_of() { cat "$work/$1.rc"; }

# 1. k6 crosses an overload threshold (rpc_errors, every error a timeout: the 10s CreateOrder timeouts of an
# overloaded machine): the attempt is an invalid attempt, the bench ends "NOT A VALID BASELINE" with exit 3
bench thr 0.5 timeouts
[ "$(rc_of thr)" = 3 ] && r=ok || r=bad; check $r "overload threshold: exit 3 (NOT A VALID BASELINE), got $(rc_of thr)"
grep -q "NOT A VALID BASELINE" "$work/thr.out/summary.md" 2>/dev/null && r=ok || r=bad; check $r "overload threshold: summary.md says NOT A VALID BASELINE"
grep -q "rpc_errors" "$work/thr.out/run1-invalid.txt" 2>/dev/null && r=ok || r=bad; check $r "overload threshold: run1-invalid.txt names the crossed threshold"
[ "$(wc -l < "$work/thr.k6" | tr -d ' ')" = 3 ] && r=ok || r=bad; check $r "overload threshold: all 3 attempts were made (each one a create phase)"

# 1b. anything that is not provably overload is a failure, never exit 3 and never "try again": a correctness
# threshold, a correctness threshold together with an overload one (correctness wins), rpc_errors that are not
# all timeouts, and a summary that is missing (fail-closed)
for mode in wrong both other nosummary onlyreq emptycount negative rpcnocount okless; do
  bench "f-$mode" 0.5 "$mode"
  rc=$(rc_of "f-$mode"); { [ "$rc" != 0 ] && [ "$rc" != 3 ]; } && r=ok || r=bad
  check $r "$mode: a failure (not exit 0, not exit 3), got exit $rc"
  [ "$(wc -l < "$work/f-$mode.k6" | tr -d ' ')" = 1 ] && r=ok || r=bad; check $r "$mode: the bench stopped at the first attempt, no retry"
  [ ! -e "$work/f-$mode.out/run1-invalid.txt" ] && r=ok || r=bad; check $r "$mode: the attempt was not written off as invalid"
done

# 1b2. the unknown-split case the reviewer reproduced ends in exactly exit 99
[ "$(rc_of f-emptycount)" = 99 ] && r=ok || r=bad; check $r "emptycount: exit 99 exactly, got $(rc_of f-emptycount)"

# 1c. a summary left in a reused out dir by an earlier bench (a clean timeouts-only one) must not be taken for
# this run's: k6 exits 99 without writing a summary, so there is none, and that is a failure
mkdir "$work/stale.out"
python3 -I - "$work/stale.out" <<'PY'
import json, sys
m = {"rpc_errors": {"count": 7, "thresholds": {"count==0": True}},
     "rpc_errors{code:request.timeout}": {"count": 7, "thresholds": {"count>=0": False}},
     "rpc_errors{code:transport.timeout}": {"count": 0, "thresholds": {"count>=0": False}}}
for n in (1, 2, 3):
    json.dump({"metrics": m}, open(f"{sys.argv[1]}/run{n}-create.json", "w"))
PY
echo 0.5 > "$work/load"; : > "$work/stale.k6"; rc=0
env PATH="$work/fakebin:$PATH" BENCH_LOADAVG_FILE="$work/load" LOAD_INTERVAL=1 RUNS=3 MAX_ATTEMPTS=3 \
  FAKE_K6=nosummary FAKE_K6_LOG="$work/stale.k6" FAKE_K6_BUSY=1000 FAKE_K6_QUIET=0.5 \
  tests/k6/bench.sh "$work/bin" "$work/stale.out" > "$work/stale.log" 2>&1 || rc=$?
{ [ "$rc" != 0 ] && [ "$rc" != 3 ]; } && r=ok || r=bad; check $r "stale summary: a failure (not exit 0, not exit 3), got exit $rc"
[ "$(wc -l < "$work/stale.k6" | tr -d ' ')" = 1 ] && r=ok || r=bad; check $r "stale summary: the bench stopped at the first attempt, no retry"

# 1c2. nor may an earlier bench's complete, VALID-looking output stand in for a k6 that exited 0 and wrote nothing
mkdir "$work/stale0.out"
python3 -I - "$work/stale0.out" <<'PY'
import json, sys
m = {"bench_iter_start_ms": {"min": 0, "max": 0}, "bench_create_end_ms": {"min": 0, "max": 1000},
     "rpc_ok{rpc:CreateOrder}": {"count": 10}, "rpc_errors": {"count": 0}, "reserved_ok": {"count": 10},
     "rpc_duration{rpc:CreateOrder}": {"med": 5.0, "p(95)": 9.0, "p(99)": 12.0},
     "order_to_reserved": {"med": 150.0, "p(95)": 250.0, "p(99)": 300.0},
     "reserved_after_create": {"med": 148.0, "p(95)": 240.0, "p(99)": 290.0}}
for n in (1, 2, 3):
    for phase in ("create", "reserve"):
        json.dump({"metrics": m}, open(f"{sys.argv[1]}/run{n}-{phase}.json", "w"))
    json.dump({"total_orders": 10, "pending_at_end": 0, "seconds": 0.1}, open(f"{sys.argv[1]}/run{n}-drain.json", "w"))
PY
echo 0.5 > "$work/load"; : > "$work/stale0.k6"; rc=0
env PATH="$work/fakebin:$PATH" BENCH_LOADAVG_FILE="$work/load" LOAD_INTERVAL=1 RUNS=3 MAX_ATTEMPTS=3 \
  FAKE_K6=okless FAKE_K6_LOG="$work/stale0.k6" FAKE_K6_BUSY=1000 FAKE_K6_QUIET=0.5 \
  tests/k6/bench.sh "$work/bin" "$work/stale0.out" > "$work/stale0.log" 2>&1 || rc=$?
{ [ "$rc" != 0 ] && [ "$rc" != 3 ]; } && r=ok || r=bad; check $r "stale valid summaries: k6 exit 0 without a summary is a failure, got exit $rc"
[ "$(wc -l < "$work/stale0.k6" | tr -d ' ')" = 1 ] && r=ok || r=bad; check $r "stale valid summaries: the bench stopped at the first attempt"

# 1d. nor may an earlier bench's other leftovers speak for this run: a stale run1-invalid.txt and a stale
# baseline sample must not make a clean attempt 1 invalid
mkdir "$work/old.out"; echo "k6 create: stale" > "$work/old.out/run1-invalid.txt"; echo "1,1000" > "$work/old.out/run1-baseline.csv"
echo 0.5 > "$work/load"; : > "$work/old.k6"; rc=0
env PATH="$work/fakebin:$PATH" BENCH_LOADAVG_FILE="$work/load" LOAD_INTERVAL=1 RUNS=3 MAX_ATTEMPTS=3 \
  FAKE_K6=ok FAKE_K6_LOG="$work/old.k6" FAKE_K6_BUSY=1000 FAKE_K6_QUIET=0.5 \
  tests/k6/bench.sh "$work/bin" "$work/old.out" > "$work/old.log" 2>&1 || rc=$?
[ "$rc" = 0 ] && r=ok || r=bad; check $r "leftovers: stale run1-invalid.txt / baseline do not invalidate attempt 1, exit 0, got $rc"

# 2. a real tool failure (k6 script error, exit 107) stays a failure: neither 0 nor 3
bench scr 0.5 script
rc=$(rc_of scr); { [ "$rc" != 0 ] && [ "$rc" != 3 ]; } && r=ok || r=bad; check $r "tool failure: exit is neither 0 nor 3 (exit $rc)"

# 3. the load the bench itself makes (1000 during the create phase, far over the limit) does not invalidate
# attempts that started on a quiet machine; it is recorded
bench own 0.5 ok
[ "$(rc_of own)" = 0 ] && r=ok || r=bad; check $r "own load: 3 valid attempts, exit 0, got $(rc_of own)"
python3 -I - "$work/own.out/result.json" <<'PY' && r=ok || r=bad
import json, sys
doc = json.load(open(sys.argv[1]))
assert doc["parameters"]["baseline_wait_s"] == 180 and doc["parameters"]["load_interval_s"] == 1, doc["parameters"]
a = doc["attempts"]
assert [x["valid"] for x in a] == [True] * 3, [x["valid"] for x in a]
assert all(x["max"] >= 1000 for x in a), "the load while the bench ran is not recorded: " + str([x["max"] for x in a])
PY
check $r "own load: result.json has the 3 attempts valid, with the load while the bench ran (>= 1000) recorded"

# 4. the machine is busy when the bench starts (1000) and quiet 2.5s later: attempt 1 waits for it
( sleep 2.5; echo 0.5 > "$work/load" ) & bg="$bg $!"
bench wait 1000 ok
python3 -I - "$work/wait.out/run1-baseline.csv" <<'PY' && r=ok || r=bad
import sys
s = [float(l.split(",")[1]) for l in open(sys.argv[1]) if l.strip()]
assert len(s) >= 3, f"{len(s)} baseline sample(s): it did not wait"
assert s[0] == 1000.0 and s[-1] == 0.5, s
PY
check $r "wait: attempt 1 sampled the busy machine until it was quiet (run1-baseline.csv)"

# 5. the machine never gets quiet: BASELINE_WAIT is up, k6 never ran, every attempt is invalid, exit 3
bench busy 1000 ok BASELINE_WAIT=2
[ ! -s "$work/busy.k6" ] && r=ok || r=bad; check $r "busy: k6 never ran on a machine that was not quiet"
[ "$(rc_of busy)" = 3 ] && r=ok || r=bad; check $r "busy: exit 3, got $(rc_of busy)"
grep -q "not quiet" "$work/busy.out/run1-invalid.txt" 2>/dev/null && r=ok || r=bad; check $r "busy: run1-invalid.txt says the environment was not quiet"

[ "$fails" = 0 ] || { echo "FAIL: $fails check(s)"; exit 1; }
echo "ok  bench overload / baseline load"
