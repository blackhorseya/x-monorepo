#!/usr/bin/env bash
# The performance baseline (#263): tests/k6/bench.js against a real ./bin/rpc + ./bin/worker +
# Postgres, RUNS times with the same fixed parameters, then tests/k6/bench_report.py folds the runs
# into result.json (machine readable) and summary.md (median and spread). `task k6-bench`.
# Not part of the PR gate and not run by CI: a shared runner's numbers are noise, a baseline is
# taken on a quiet machine you name in the report (docs/perf-baseline.md).
#   bench.sh <bin dir> [out dir]       out dir default: <bin dir>/k6-bench/<stamp>
#
# Every run gets its own database (k6_bench_<stamp>_<n>, created on the server of DATABASE_URL and
# migrated by ./bin/migrate) and its own RUN_ID, and the database is dropped when the run ends, also
# on failure or ^C: the write volume is bounded by the parameters below and nothing is left behind,
# and every run starts from the same empty outbox. Needs psql and a role that may CREATE DATABASE.
#
# Fixed parameters (override only to measure something else; they are recorded in result.json):
#   RUNS=3 (N, ≥ 3)          CREATE_VUS=20 CREATE_DURATION=10s      create phase: closed model, saturation
#   RESERVE_RATE=100 RESERVE_DURATION=30s                           reserve phase: open model, orders/s
#   SKUS=50  POLL=50 (ms)  DRAIN_TIMEOUT=300 (s)  PORT=18090
# The k6 scripts never print a response body or URL; to prove the key stays out, the rpc is sent a
# made-up key as API_KEY / ADMIN_API_KEY (auth is off, it is ignored) and every file the run
# produces is searched for it; one hit fails the whole bench.
set -euo pipefail
bin=$1; shift
stamp=$(date -u +%Y%m%dT%H%M%SZ)
out=${1:-$bin/k6-bench/$stamp}
mkdir -p "$out"

RUNS=${RUNS:-3}
[ "$RUNS" -ge 3 ] || { echo "RUNS=$RUNS: want at least 3" >&2; exit 2; }
CREATE_VUS=${CREATE_VUS:-20}; CREATE_DURATION=${CREATE_DURATION:-10s}
RESERVE_RATE=${RESERVE_RATE:-100}; RESERVE_DURATION=${RESERVE_DURATION:-30s}
SKUS=${SKUS:-50}; POLL=${POLL:-50}; DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-300}
port=${PORT:-18090}
base_url=${DATABASE_URL:-postgres://app:app@127.0.0.1:5432/app?sslmode=disable}
CANARY=k6-fake-bench-canary-0001   # made up for this test; nothing here is a credential

# <base url> <database> → the same server and options, another database
dburl() { python3 -I -c 'import sys, urllib.parse as u
p = u.urlsplit(sys.argv[1]); print(u.urlunsplit(p._replace(path="/" + sys.argv[2])))' "$base_url" "$1"; }

export APP_CONFIG=/dev/null APP_HTTP_ADDR=":$port" APP_HTTP_DOCS=false APP_LOG_LEVEL=${APP_LOG_LEVEL:-warn}

pid="" wpid="" db=""
cleanup() {
  [ -n "$wpid" ] && kill "$wpid" 2>/dev/null && wait "$wpid" 2>/dev/null || true; wpid=""
  [ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null || true; pid=""
  if [ -n "$db" ]; then
    psql "$base_url" -qAt -c "drop database if exists $db with (force)" >/dev/null 2>&1 \
      || echo "WARNING: could not drop $db: drop it by hand" >&2
    db=""
  fi
}
trap cleanup EXIT INT TERM

for n in $(seq 1 "$RUNS"); do
  db="k6_bench_$(printf %s "$stamp" | tr A-Z a-z)_$n"
  run_id="bench-$stamp-$n"
  url=$(dburl "$db")
  echo "== run $n/$RUNS ($run_id, database $db)"
  psql "$base_url" -qAt -c "create database $db" >/dev/null
  APP_POSTGRES_URL=$url "$bin/migrate" up > "$out/run$n-migrate.log" 2>&1
  APP_POSTGRES_URL=$url "$bin/rpc" > "$out/run$n-rpc.log" 2>&1 & pid=$!
  APP_POSTGRES_URL=$url "$bin/worker" > "$out/run$n-worker.log" 2>&1 & wpid=$!
  for _ in $(seq 1 100); do
    kill -0 "$pid" 2>/dev/null || { echo "rpc exited:"; cat "$out/run$n-rpc.log"; exit 1; }
    kill -0 "$wpid" 2>/dev/null || { echo "worker exited:"; cat "$out/run$n-worker.log"; exit 1; }
    curl -fsS "http://127.0.0.1:$port/api/readyz" >/dev/null 2>&1 && break
    sleep 0.2
  done

  k6env=(-e "BASE_URL=http://127.0.0.1:$port" -e "API_KEY=$CANARY" -e "ADMIN_API_KEY=$CANARY" -e "RUN_ID=$run_id"
    -e "SKUS=$SKUS" -e "POLL=$POLL")
  k6 run --quiet --summary-export "$out/run$n-create.json" tests/k6/bench.js "${k6env[@]}" \
    -e PHASE=create -e "VUS=$CREATE_VUS" -e "DURATION=$CREATE_DURATION" > "$out/run$n-create.log" 2>&1

  # the create phase leaves a backlog for the worker: wait for it, and time it (reservation
  # throughput of the batching worker with nothing else going on). PENDING = not yet reserved.
  t0=$SECONDS
  until [ "$(psql "$url" -qAt -c "select count(*) from ordering.orders where status = 'PENDING'")" = 0 ]; do
    [ $((SECONDS - t0)) -lt "$DRAIN_TIMEOUT" ] || { echo "backlog not drained within ${DRAIN_TIMEOUT}s" >&2; exit 1; }
    sleep 0.5
  done
  drained=$(psql "$url" -qAt -c "select count(*) from ordering.orders")
  echo "{\"orders\": $drained, \"seconds\": $((SECONDS - t0))}" > "$out/run$n-drain.json"

  k6 run --quiet --summary-export "$out/run$n-reserve.json" tests/k6/bench.js "${k6env[@]}" \
    -e PHASE=reserve -e "RATE=$RESERVE_RATE" -e "DURATION=$RESERVE_DURATION" > "$out/run$n-reserve.log" 2>&1
  cleanup
done

# where the numbers were taken: enough for someone else to rerun them under the same conditions
cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo)
ncpu=$(sysctl -n hw.ncpu 2>/dev/null || nproc)
pgv=$(psql "$base_url" -qAt -c "show server_version")
BENCH_COMMIT=$(git rev-parse HEAD) BENCH_DIRTY=$(git status --porcelain -- . ':!bin' | head -c1) \
BENCH_GO=$(go version) BENCH_PG=$pgv BENCH_K6=$(k6 version | head -n1) BENCH_CPU=$cpu BENCH_NCPU=$ncpu \
BENCH_OS=$(uname -srm) BENCH_STAMP=$stamp BENCH_RUNS=$RUNS BENCH_SKUS=$SKUS BENCH_POLL=$POLL \
BENCH_CREATE_VUS=$CREATE_VUS BENCH_CREATE_DURATION=$CREATE_DURATION \
BENCH_RESERVE_RATE=$RESERVE_RATE BENCH_RESERVE_DURATION=$RESERVE_DURATION \
  python3 -I tests/k6/bench_report.py "$out"

# R4: the key must be in nothing this run wrote
if grep -rl -- "$CANARY" "$out" 2>/dev/null; then
  echo "FAIL: the API key appears in the files above" >&2
  exit 1
fi
echo "ok  no API key in $out"
cat "$out/summary.md"
