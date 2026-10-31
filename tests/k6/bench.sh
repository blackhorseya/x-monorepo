#!/usr/bin/env bash
# The performance baseline (#263): tests/k6/bench.js against a real ./bin/rpc + ./bin/worker +
# Postgres, RUNS times with the same fixed parameters, then tests/k6/bench_report.py folds the runs
# into result.json (machine readable) and summary.md (median and spread). `task k6-bench`.
# Not part of the PR gate and not run by CI: a shared runner's numbers are noise, a baseline is
# taken on a quiet machine you name in the report (docs/perf-baseline.md).
#
# Which attempts count is decided here, by a rule fixed before the run, not by whoever reads the numbers:
# before each attempt the 1m load average is sampled (every LOAD_INTERVAL=5s, into run<n>-baseline.csv) until it
# is <= MAX_LOAD_PER_CORE x cores, for at most BASELINE_WAIT=180s; an attempt is valid iff it started on such a
# quiet machine and ran to the end. The load while an attempt runs is the bench's own (rpc, worker, Postgres and
# k6 share the machine): it is sampled into run<n>-load.csv and reported, never judged. Two things end an attempt
# as INVALID instead of the bench as failed, both written to run<n>-invalid.txt: the machine never got quiet
# (the attempt is not run), and k6 crossing an OVERLOAD threshold (exit 99; only reservation_timeouts and rpc_errors
# that are all timeouts count, judged from the k6 summary by a name list in bench_report.py; a correctness threshold
# such as reservation_wrong_status, an unknown one or an unreadable summary is a failure, exit 99, never exit 3). Any other failure (no binary, migrate, a k6 script error, drain) is the tool's
# and ends the bench with a non-0, non-3 exit. Attempts go on until RUNS valid ones exist or MAX_ATTEMPTS is
# reached; every attempt stays in the output directory; the statistics use the valid ones only; fewer than
# RUNS valid = "NOT A VALID BASELINE" and exit 3 (the numbers that exist are still printed, under that banner).
#   bench.sh <bin dir> [out dir]       out dir default: <bin dir>/k6-bench/<stamp>
#
# Every run gets its own database (k6_bench_<stamp>_<pid+random>_<n>, created on the server of DATABASE_URL and
# migrated by ./bin/migrate) and its own RUN_ID, and the database is dropped when the run ends, also
# on failure or ^C — but only if this run's own CREATE DATABASE succeeded: a database that already has
# the name is someone else's, the run fails there and never drops it (tests/k6/bench_db_test.sh,
# `task k6-bench-test`). The write volume is bounded by the parameters below and nothing is left behind,
# and every run starts from the same empty outbox. Needs psql and a role that may CREATE DATABASE.
#
# Fixed parameters (override only to measure something else; they are recorded in result.json):
#   RUNS=3 (valid runs wanted, ≥ 3)  MAX_ATTEMPTS=6  MAX_LOAD_PER_CORE=1.0  (BASELINE_WAIT=180 s, LOAD_INTERVAL=5 s)
#   CREATE_VUS=20 CREATE_DURATION=10s      create phase: closed model, saturation
#   RESERVE_RATE=100 RESERVE_DURATION=30s                           reserve phase: open model, orders/s
#   SKUS=50  SEED=1 (SKU choice)  POLL=50 (ms)  DRAIN_TIMEOUT=300 (s)  PORT=18090
# The k6 scripts never print a response body or URL; to prove the key stays out, the rpc is sent a
# made-up key as API_KEY / ADMIN_API_KEY (auth is off, it is ignored) and every file the run
# produces is searched for it; one hit fails the whole bench.
set -euo pipefail
bin=$1; shift
stamp=$(date -u +%Y%m%dT%H%M%SZ)
# pid + random: two benches started in the same second (same stamp) never share a database name or
# an output directory. BENCH_DB_SUFFIX pins it (bench_db_test.sh forces a collision with it).
suffix=${BENCH_DB_SUFFIX:-$$$RANDOM}
out=${1:-$bin/k6-bench/$stamp-$suffix}
mkdir -p "$out"

RUNS=${RUNS:-3}
[ "$RUNS" -ge 3 ] || { echo "RUNS=$RUNS: want at least 3" >&2; exit 2; }
CREATE_VUS=${CREATE_VUS:-20}; CREATE_DURATION=${CREATE_DURATION:-10s}
RESERVE_RATE=${RESERVE_RATE:-100}; RESERVE_DURATION=${RESERVE_DURATION:-30s}
MAX_ATTEMPTS=${MAX_ATTEMPTS:-$((RUNS * 2))}; MAX_LOAD_PER_CORE=${MAX_LOAD_PER_CORE:-1.0}
LOAD_INTERVAL=${LOAD_INTERVAL:-5}; BASELINE_WAIT=${BASELINE_WAIT:-180}
SKUS=${SKUS:-50}; POLL=${POLL:-50}; SEED=${SEED:-1}; DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-300}
port=${PORT:-18090}
base_url=${DATABASE_URL:-postgres://app:app@127.0.0.1:5432/app?sslmode=disable}
CANARY=k6-fake-bench-canary-0001   # made up for this test; nothing here is a credential

# <base url> <database> → the same server and options, another database
dburl() { python3 -I -c 'import sys, urllib.parse as u
p = u.urlsplit(sys.argv[1]); print(u.urlunsplit(p._replace(path="/" + sys.argv[2])))' "$base_url" "$1"; }

export APP_CONFIG=/dev/null APP_HTTP_ADDR=":$port" APP_HTTP_DOCS=false APP_LOG_LEVEL=${APP_LOG_LEVEL:-warn}
# shellcheck source=tests/k6/catalog.sh
. tests/k6/catalog.sh   # rpc has no catalog of its own: a catalog-rpc per attempt, on that attempt's database

ncpu=$(sysctl -n hw.ncpu 2>/dev/null || nproc)
limit=$(python3 -I -c "print($MAX_LOAD_PER_CORE * $ncpu)")

# 1m load average every LOAD_INTERVAL seconds into $1 (epoch,load) until stopped (tests/k6/loadavg.py)
pid="" wpid="" db="" lpid=""
sample_load() { python3 -I tests/k6/loadavg.py "$LOAD_INTERVAL" >> "$1" & lpid=$!; }
stop_load() { [ -n "$lpid" ] && kill "$lpid" 2>/dev/null && wait "$lpid" 2>/dev/null || true; lpid=""; }
# <attempt>: valid iff the machine was quiet before it and bench.sh did not cut it short (bench_report.py's rule, same code)
attempt_valid() {
  python3 -I -c 'import sys; sys.path.insert(0, "tests/k6"); import bench_report as r
sys.exit(0 if r.attempt_status(sys.argv[1], sys.argv[2], float(sys.argv[3]))["valid"] else 1)' "$out" "$1" "$limit"
}
# <attempt>: sample the load into run<n>-baseline.csv until the last sample is <= the limit, for at most BASELINE_WAIT seconds
wait_quiet() {
  local f="$out/run$1-baseline.csv" t0=$SECONDS last
  while :; do
    python3 -I tests/k6/loadavg.py >> "$f"
    last=$(tail -n 1 "$f" | cut -d, -f2)
    python3 -I -c "import sys; sys.exit(0 if $last <= $limit else 1)" && return 0
    [ $((SECONDS - t0)) -lt "$BASELINE_WAIT" ] || return 1
    sleep "$LOAD_INTERVAL"
  done
}
# <attempt> <phase> <k6 args…>: a crossed threshold (exit 99) is looked at (bench_report.py classify_threshold_failure,
# docs/perf-baseline.md): provably overload (timeouts) sets $overload and returns 1 — the attempt is invalid, the
# tool is not broken; every other crossing, and anything the classifier cannot read, is a failure (exit 99, fail-closed).
# Any other k6 failure ends the bench too.
k6_phase() {
  local n=$1 phase=$2 rc=0 verdict; shift 2
  k6 run --quiet --summary-export "$out/run$n-$phase.json" tests/k6/bench.js "${k6env[@]}" -e "PHASE=$phase" "$@" \
    > "$out/run$n-$phase.log" 2>&1 || rc=$?
  if [ "$rc" = 0 ]; then  # exit 0 is only a success with this run's summary: the file was removed before the attempt
    [ -s "$out/run$n-$phase.json" ] || { echo "bench failed: k6 $phase exited 0 but wrote no summary (run$n-$phase.json)" >&2; exit 1; }
    return 0
  fi
  if [ "$rc" != 99 ]; then echo "k6 $phase failed (exit $rc):" >&2; tail -n 5 "$out/run$n-$phase.log" >&2; exit "$rc"; fi
  verdict=$(python3 -I -c 'import sys; sys.path.insert(0, "tests/k6"); import bench_report as r
kind, why = r.classify_threshold_failure(sys.argv[1], sys.argv[2]); print(kind, why)' "$out/run$n-$phase.json" "$phase" 2>&1) \
    || verdict="failure k6 $phase exited 99 and the classifier failed: $verdict"
  case "$verdict" in
    "overload "*) overload=${verdict#overload }; return 1 ;;
    *) echo "bench failed: ${verdict#failure }" >&2; exit 99 ;;
  esac
}

cleanup() {
  stop_load
  catalog_stop
  [ -n "$wpid" ] && kill "$wpid" 2>/dev/null && wait "$wpid" 2>/dev/null || true; wpid=""
  [ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null || true; pid=""
  if [ -n "$db" ]; then
    psql "$base_url" -qAt -c "drop database if exists $db with (force)" >/dev/null 2>&1 \
      || echo "WARNING: could not drop $db: drop it by hand" >&2
    db=""
  fi
}
trap cleanup EXIT INT TERM

valid=0
attempts=0
while [ "$valid" -lt "$RUNS" ] && [ "$attempts" -lt "$MAX_ATTEMPTS" ]; do
  attempts=$((attempts + 1)); n=$attempts
  name="k6_bench_$(printf %s "$stamp" | tr A-Z a-z)_${suffix}_$n"
  run_id="bench-$stamp-$suffix-$n"
  url=$(dburl "$name")
  echo "== attempt $n ($valid/$RUNS valid so far; $run_id, database $name)"
  # whatever an earlier bench left under this attempt's names (a reused out dir) is not this run's evidence
  rm -f "$out"/run"$n"-*
  if ! wait_quiet "$n"; then
    echo "environment not quiet: 1m load $(tail -n 1 "$out/run$n-baseline.csv" | cut -d, -f2) > $limit after waiting ${BASELINE_WAIT}s" > "$out/run$n-invalid.txt"
    echo "   attempt $n INVALID ($(cat "$out/run$n-invalid.txt")): not run, kept, not counted"
    continue
  fi
  sample_load "$out/run$n-load.csv"
  # db (what cleanup drops) is set only once this CREATE succeeded; if it fails the name was taken
  psql "$base_url" -qAt -c "create database $name" >/dev/null || { echo "cannot create $name: not touching it" >&2; exit 1; }
  db=$name
  APP_POSTGRES_URL=$url "$bin/migrate" up > "$out/run$n-migrate.log" 2>&1
  APP_POSTGRES_URL=$url catalog_start "$bin" "$((port + 2))" "$out/run$n-catalog.log"
  APP_POSTGRES_URL=$url "$bin/rpc" > "$out/run$n-rpc.log" 2>&1 & pid=$!
  APP_POSTGRES_URL=$url "$bin/worker" > "$out/run$n-worker.log" 2>&1 & wpid=$!
  for _ in $(seq 1 100); do
    kill -0 "$pid" 2>/dev/null || { echo "rpc exited:"; cat "$out/run$n-rpc.log"; exit 1; }
    kill -0 "$wpid" 2>/dev/null || { echo "worker exited:"; cat "$out/run$n-worker.log"; exit 1; }
    curl -fsS "http://127.0.0.1:$port/api/readyz" >/dev/null 2>&1 && break
    sleep 0.2
  done

  k6env=(-e "BASE_URL=http://127.0.0.1:$port" -e "API_KEY=$CANARY" -e "ADMIN_API_KEY=$CANARY" -e "RUN_ID=$run_id"
    -e "SKUS=$SKUS" -e "POLL=$POLL" -e "SEED=$SEED")
  overload=""
  if k6_phase "$n" create -e "VUS=$CREATE_VUS" -e "DURATION=$CREATE_DURATION"; then
    # the create phase leaves a backlog for the worker (CreateOrder outruns reservation): how long it
    # takes to drain, over the orders still PENDING at the start of that window (tests/k6/bench_drain.sh)
    tests/k6/bench_drain.sh "$url" "$out/run$n-drain.json" "$DRAIN_TIMEOUT"
    k6_phase "$n" reserve -e "RATE=$RESERVE_RATE" -e "DURATION=$RESERVE_DURATION" || true
  fi
  stop_load
  python3 -I tests/k6/loadavg.py >> "$out/run$n-load.csv"
  cleanup
  if [ -n "$overload" ]; then
    echo "$overload" > "$out/run$n-invalid.txt"
    echo "   attempt $n INVALID ($overload): kept, not counted"
  elif attempt_valid "$n"; then
    valid=$((valid + 1)); echo "   attempt $n valid"
  else
    echo "   attempt $n INVALID (load before it over $limit): kept, not counted"
  fi
done

# where the numbers were taken: enough for someone else to rerun them under the same conditions
cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo)
rc=0
pgv=$(psql "$base_url" -qAt -c "show server_version")
BENCH_COMMIT=$(git rev-parse HEAD) BENCH_DIRTY=$(git status --porcelain -- . ':!bin' | head -c1) \
BENCH_GO=$(go version) BENCH_PG=$pgv BENCH_K6=$(k6 version | head -n1) BENCH_CPU=$cpu BENCH_NCPU=$ncpu \
BENCH_OS=$(uname -srm) BENCH_STAMP=$stamp BENCH_ATTEMPTS=$attempts BENCH_MAX_LOAD_PER_CORE=$MAX_LOAD_PER_CORE BENCH_BASELINE_WAIT=$BASELINE_WAIT BENCH_LOAD_INTERVAL=$LOAD_INTERVAL BENCH_RUNS=$RUNS BENCH_SKUS=$SKUS BENCH_POLL=$POLL BENCH_SEED=$SEED \
BENCH_CREATE_VUS=$CREATE_VUS BENCH_CREATE_DURATION=$CREATE_DURATION \
BENCH_RESERVE_RATE=$RESERVE_RATE BENCH_RESERVE_DURATION=$RESERVE_DURATION \
  python3 -I tests/k6/bench_report.py "$out" || rc=$?

# R4: the key must be in nothing this run wrote
if grep -rl -- "$CANARY" "$out" 2>/dev/null; then
  echo "FAIL: the API key appears in the files above" >&2
  exit 1
fi
echo "ok  no API key in $out"
cat "$out/summary.md"
exit "$rc"
