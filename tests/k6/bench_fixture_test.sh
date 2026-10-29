#!/usr/bin/env bash
# bench.js against tests/k6/bench_stub.py (no Postgres, no rpc): the properties of its numbers that a
# real run cannot isolate. `task k6-bench-test`; not part of CI.
#   BENCH_JS=<script>  runs another copy of bench.js (to see the checks go red on an older one)
set -euo pipefail
js=${BENCH_JS:-tests/k6/bench.js}
port=${PORT:-18290}
work=$(mktemp -d)
xpid=""
stop() { [ -n "$xpid" ] && kill "$xpid" 2>/dev/null && wait "$xpid" 2>/dev/null || true; xpid=""; }
trap 'stop; rm -rf "$work"' EXIT

fails=0
check() { if [ "$1" = ok ]; then echo "--- PASS: $2"; else echo "--- FAIL: $2"; fails=$((fails + 1)); fi; }

# run <name> <stub env…> -- <k6 -e args…>: stub up, one k6 run, stub down; sku log and summary in $work
run() {
  local name=$1; shift
  local stubenv=() kargs=()
  while [ "$1" != -- ]; do stubenv+=("$1"); shift; done; shift
  kargs=("$@")
  : > "$work/$name.skus"
  env "${stubenv[@]}" STUB_SKU_LOG="$work/$name.skus" python3 tests/k6/bench_stub.py "$port" & xpid=$!
  for _ in $(seq 1 50); do (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && break; sleep 0.1; done
  local rc=0
  k6 run --quiet --summary-export "$work/$name.json" "$js" -e "BASE_URL=http://127.0.0.1:$port" -e RUN_ID=fx \
    -e K6_CUSTOMER_MODE=omit "${kargs[@]}" > "$work/$name.log" 2>&1 || rc=$?
  echo "$rc" > "$work/$name.rc"
  stop
}

# 1. same parameters → same SKUs in the same order (one VU), another SEED → another sequence
run a1 SETUP_DELAY_MS=0 -- -e PHASE=create -e VUS=1 -e DURATION=2s -e SEED=7
run a2 SETUP_DELAY_MS=0 -- -e PHASE=create -e VUS=1 -e DURATION=2s -e SEED=7
run a3 SETUP_DELAY_MS=0 -- -e PHASE=create -e VUS=1 -e DURATION=2s -e SEED=8
n=$(wc -l < "$work/a1.skus"); m=$(wc -l < "$work/a2.skus"); k=$((n < m ? n : m))
[ "$k" -ge 20 ] && r=ok || r=bad; check $r "seed: enough SKUs to compare ($k)"
[ "$(head -n "$k" "$work/a1.skus")" = "$(head -n "$k" "$work/a2.skus")" ] && r=ok || r=bad
check $r "seed: two runs with SEED=7 ask for the same SKUs in the same order"
[ "$(head -n "$k" "$work/a1.skus")" != "$(head -n "$k" "$work/a3.skus")" ] && r=ok || r=bad
check $r "seed: SEED=8 asks for a different sequence"

# 2. CreateOrder takes 500ms: the end-to-end latency includes it, the worker's part does not
run b1 CREATE_DELAY_MS=500 -- -e PHASE=reserve -e RATE=2 -e DURATION=3s -e PRE_VUS=5
python3 -I - "$work/b1.json" <<'PY' && r=ok || r=bad
import json, sys
m = json.load(open(sys.argv[1]))["metrics"]
e2e, worker = m["order_to_reserved"]["med"], m["reserved_after_create"]["med"]
assert e2e >= 500, f"order_to_reserved med {e2e}ms: CreateOrder's 500ms is missing"
assert worker < 200, f"reserved_after_create med {worker}ms: should not include CreateOrder"
PY
check $r "latency: order_to_reserved includes a 500ms CreateOrder, reserved_after_create does not"

# 3. setup() takes ~1s (50 SetStock × 20ms): throughput is over the scenario's window, not the test's
run c1 SETUP_DELAY_MS=20 -- -e PHASE=create -e VUS=1 -e DURATION=1s -e SKUS=50
python3 -I - "$work/c1.json" <<'PY' && r=ok || r=bad
import json, os, sys
sys.path.insert(0, "tests/k6")
import bench_report as br
s = json.load(open(sys.argv[1]))
count = s["metrics"]["rpc_ok{rpc:CreateOrder}"]["count"]
t = br.throughput(s, "CreateOrder")
assert count > 20, f"only {count} CreateOrders"
assert t >= 0.75 * count, f"throughput {t:.1f}/s for {count} CreateOrders in a ~1s scenario"
PY
check $r "throughput: not diluted by a 1s setup()"

# 4. a created order whose reservation never shows up (CreateOrder takes 6.5s, the wait is 1s, the test
# lasts 1s): the iteration is not cut off by gracefulStop but runs to its own timeout, and k6 fails the
# run (reservation_timeouts). With the old gracefulStop it was cut off: k6 exit 0, nothing counted.
run d1 CREATE_DELAY_MS=6500 NEVER_RESERVE=1 -- -e PHASE=reserve -e RATE=1 -e DURATION=1s -e PRE_VUS=1 -e MAX_VUS=1 -e RESERVE_TIMEOUT=1000
python3 -I - "$work/d1.json" "$(cat "$work/d1.rc")" <<'PY' && r=ok || r=bad
import json, sys
rc = int(sys.argv[2])
m = json.load(open(sys.argv[1]))["metrics"]
timeouts = m["reservation_timeouts"]["count"]
assert rc != 0 and timeouts == 1, f"k6 exit {rc}, reservation_timeouts {timeouts}: an unfinished order passed as a success"
PY
check $r "interrupted: an order created but never RESERVED does not pass quietly"

[ "$fails" = 0 ] || { echo "FAIL: $fails check(s)"; exit 1; }
echo "ok  bench fixtures"
