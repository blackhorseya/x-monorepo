#!/usr/bin/env bash
# One measurement of worker.subscribe_batch_size (#363 slice 2), each in its own database that is
# created on the server of DATABASE_URL, migrated, and dropped when the run ends.
#   subscribe_batch.sh <bin dir> <A|B> <batch size> <replicas> <out json> [worker bin dir]
#
#   A  reservation_latency p95 of `k6 run tests/k6/reservation.js` (VUS=50, DURATION=60s) with the worker
#      running. With BATCHSTAT_FILE exported and an instrumented worker (see docs/operations.md) it also
#      proves how many rows each poll claimed.
#   B  seconds from starting the worker(s) to "no order PENDING, every order CONFIRMED with a RESERVED
#      reservation" (tests/k6/bench_drain.sh), over a backlog of ORDERS (2000) orders created while
#      no worker ran. <replicas> workers in the same consumer group.
#
# Fixed on purpose: everything but the batch size and the replica count (poll_interval 200ms default,
# SKUS=20, POLL=100, PORT=18190). Writes {"metric","batch","replicas","value","load_before","load_after",...}.
set -euo pipefail
bin=$1 metric=$2 batch=$3 replicas=$4 out=$5 wbin=${6:-$1}
ORDERS=${ORDERS:-2000}; VUS=${VUS:-50}; DURATION=${DURATION:-60s}; port=${PORT:-18190}
base_url=${DATABASE_URL:-postgres://app:app@127.0.0.1:5432/app?sslmode=disable}
CANARY=k6-fake-sbatch-canary-0001
dburl() { python3 -I -c 'import sys, urllib.parse as u
p = u.urlsplit(sys.argv[1]); print(u.urlunsplit(p._replace(path="/" + sys.argv[2])))' "$base_url" "$1"; }

export APP_CONFIG=/dev/null APP_HTTP_ADDR=":$port" APP_HTTP_DOCS=false APP_LOG_LEVEL=${APP_LOG_LEVEL:-warn}
# shellcheck source=tests/k6/catalog.sh
. tests/k6/catalog.sh

name="sbatch_$(date -u +%Y%m%d%H%M%S)_$$"
url=$(dburl "$name")
pid="" wpids="" db=""
cleanup() {
  catalog_stop
  # shellcheck disable=SC2086
  [ -z "$wpids" ] || kill $wpids 2>/dev/null || true
  [ -z "$pid" ] || kill "$pid" 2>/dev/null || true
  wait 2>/dev/null || true
  [ -z "$db" ] || psql "$base_url" -qAt -c "drop database if exists $db with (force)" >/dev/null 2>&1 \
    || echo "WARNING: could not drop $db: drop it by hand" >&2
}
trap cleanup EXIT INT TERM

load() { python3 -I tests/k6/loadavg.py | cut -d, -f2; }
start_worker() {
  APP_POSTGRES_URL=$url APP_WORKER_SUBSCRIBE_BATCH_SIZE=$batch "$wbin/worker" >> "$out.worker.log" 2>&1 & wpids="$wpids $!"
}
k6env=(-e "BASE_URL=http://127.0.0.1:$port" -e "API_KEY=$CANARY" -e "ADMIN_API_KEY=$CANARY" -e "SKUS=${SKUS:-20}" -e "POLL=${POLL:-100}")

psql "$base_url" -qAt -c "create database $name" >/dev/null
db=$name
APP_POSTGRES_URL=$url "$bin/migrate" up > "$out.migrate.log" 2>&1
APP_POSTGRES_URL=$url catalog_start "$bin" "$((port + 2))" "$out.catalog.log"
APP_POSTGRES_URL=$url "$bin/rpc" > "$out.rpc.log" 2>&1 & pid=$!
for _ in $(seq 1 100); do
  kill -0 "$pid" 2>/dev/null || { echo "rpc exited:"; cat "$out.rpc.log"; exit 1; }
  curl -fsS "http://127.0.0.1:$port/api/readyz" >/dev/null 2>&1 && break
  sleep 0.2
done

before=$(load)
case "$metric" in
A)
  for _ in $(seq 1 "$replicas"); do start_worker; done
  sleep 2 # workers connected and polling
  k6 run --quiet --summary-export "$out.k6.json" tests/k6/reservation.js "${k6env[@]}" -e "VUS=$VUS" -e "DURATION=$DURATION" -e RESERVE_P95=3600000 > "$out.k6.log" 2>&1 \
    || { echo "k6 failed:"; tail -n 15 "$out.k6.log"; exit 1; }
  value=$(jq '.metrics.reservation_latency["p(95)"]' "$out.k6.json")
  ;;
B)
  k6 run --quiet tests/k6/backlog.js "${k6env[@]}" -e "ORDERS=$ORDERS" > "$out.k6.log" 2>&1 \
    || { echo "k6 failed:"; tail -n 15 "$out.k6.log"; exit 1; }
  [ "$(psql "$url" -qAt -c "select count(*) from ordering.orders where status = 'PENDING'")" = "$ORDERS" ] \
    || { echo "backlog is not $ORDERS PENDING orders" >&2; exit 1; }
  for _ in $(seq 1 "$replicas"); do start_worker; done
  tests/k6/bench_drain.sh "$url" "$out.drain.json" 600
  [ "$(jq .total_orders "$out.drain.json")" = "$ORDERS" ] || { echo "drained $(jq .total_orders "$out.drain.json") orders, want $ORDERS" >&2; exit 1; }
  value=$(jq .seconds "$out.drain.json")
  ;;
*) echo "metric: want A or B" >&2; exit 2 ;;
esac
after=$(load)
echo "{\"metric\": \"$metric\", \"batch\": $batch, \"replicas\": $replicas, \"value\": $value, \"load_before\": $before, \"load_after\": $after}" > "$out"
cat "$out"
if grep -rl -- "$CANARY" "$out".* 2>/dev/null; then echo "FAIL: the API key appears in the files above" >&2; exit 1; fi
