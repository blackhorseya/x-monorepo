#!/usr/bin/env bash
# Runs each k6 script against its own ./bin/rpc (built already) on $PORT with the Postgres
# at $DATABASE_URL: orders under its load thresholds, ratelimit with the server at 50 rps,
# timeout with the server at a 1ns deadline, reservation with a ./bin/worker next to the
# rpc (outbox → worker → RESERVED latency). `task k6-smoke` locally, the `k6` job in CI.
#   smoke.sh <bin dir> [script…]        (default: orders ratelimit timeout reservation)
set -euo pipefail
bin=$1; shift
scripts=("$@"); [ ${#scripts[@]} -gt 0 ] || scripts=(orders ratelimit timeout reservation)
port=${PORT:-18090}
export APP_CONFIG=configs/config.example.yaml APP_HTTP_ADDR=":$port" APP_HTTP_DOCS=false
export APP_POSTGRES_URL=${DATABASE_URL:-postgres://app:app@127.0.0.1:5432/app?sslmode=disable}
export APP_LOG_LEVEL=${APP_LOG_LEVEL:-warn}

pid="" wpid=""
stop() {
  # worker first: SIGTERM drains in-flight handlers (≤ 8s) and exits 0.
  [ -n "$wpid" ] && kill "$wpid" 2>/dev/null && wait "$wpid" 2>/dev/null || true; wpid=""
  [ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null || true; pid=""
}
trap stop EXIT

for s in "${scripts[@]}"; do
  echo "== $s"
  env=()
  case $s in
    ratelimit) env=(APP_HTTP_RATE_LIMIT_RPS=50 APP_HTTP_RATE_LIMIT_BURST=50) ;;
    timeout)   env=(APP_HTTP_TIMEOUT=1ns APP_LOG_LEVEL=error) ;;
  esac
  env ${env[@]+"${env[@]}"} "$bin/rpc" > "$bin/k6-$s.log" 2>&1 & pid=$!
  if [ "$s" = reservation ]; then
    # Defaults, not config.example.yaml: that file has redis.url for compose-up and
    # the worker pings whatever redis.url names at startup even with broker=postgres —
    # CI has no Redis. Same as the console e2e's worker.setup.ts.
    APP_CONFIG=/dev/null "$bin/worker" > "$bin/k6-$s-worker.log" 2>&1 & wpid=$!
  fi
  for _ in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || { echo "rpc exited:"; cat "$bin/k6-$s.log"; exit 1; }
    curl -fsS "http://127.0.0.1:$port/api/readyz" >/dev/null 2>&1 && break
    sleep 0.2
  done
  # CREATE_P95: the smoke runs on a shared CI runner against a Postgres container, where
  # CreateOrder's p95 lands anywhere between 150 and 400ms at the same p50 as a laptop.
  # RESERVE_P95: same story for the worker chain (outbox poll + handler + the Postgres service).
  k6 run --quiet "tests/k6/$s.js" -e "BASE_URL=http://127.0.0.1:$port" -e VUS=10 -e DURATION=10s -e RPS=50 \
    -e "CREATE_P95=${CREATE_P95:-500}" -e "RESERVE_P95=${RESERVE_P95:-5000}" -e "RUN_ID=smoke-$$"
  stop
done
