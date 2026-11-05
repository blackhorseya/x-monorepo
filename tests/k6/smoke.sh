#!/usr/bin/env bash
# Runs each k6 script against its own ./bin/rpc (built already) with the Postgres at $DATABASE_URL:
# orders under its load thresholds, ratelimit with the server at 50 rps, timeout with the server at
# a 1ns deadline, reservation with a ./bin/worker next to the rpc (outbox → worker → RESERVED
# latency). Each script also gets its own ./bin/catalog-rpc (rpc has no catalog of its own:
# tests/k6/catalog.sh). `task k6-smoke` locally, the `k6` job in CI.
#   smoke.sh <bin dir> [script…]        (default: reservation orders ratelimit timeout)
#
# Order: reservation runs ALONE and first, so its worker meets an empty outbox, not the backlog the
# others leave behind. Everything else then runs at the same time (#451). What each script shares
# with the others, and how it is kept apart:
#   Postgres   shared (one database). orders writes orders + outbox rows; ratelimit/timeout read or
#              get a 504 before touching rows. Nothing but reservation has a worker, and reservation
#              is finished (worker stopped) before the others start, so no worker ever sees their rows.
#   outbox     reservation first, alone: the empty-outbox guarantee is unchanged.
#   rpc port   $PORT + 10 × (position in the list); the catalog-rpc is that port + 2.
#   catalog    one catalog-rpc per script (catalog.sh exports APP_CATALOG_* into the shell that calls it,
#              so each script runs in a subshell of its own).
#   RUN_ID     smoke-$$-<script>: SKUs and customers are named from it.
#   logs       $bin/k6-<script>*.log; k6 output is buffered per script and printed in list order.
set -euo pipefail
bin=$1; shift
scripts=("$@"); [ ${#scripts[@]} -gt 0 ] || scripts=(reservation orders ratelimit timeout)
port=${PORT:-18090}
export APP_CONFIG=configs/config.example.yaml APP_HTTP_ADDR=":$port" APP_HTTP_DOCS=false
export APP_POSTGRES_URL=${DATABASE_URL:-postgres://app:app@127.0.0.1:5432/app?sslmode=disable}
export APP_LOG_LEVEL=${APP_LOG_LEVEL:-warn}
# shellcheck source=tests/k6/catalog.sh
. tests/k6/catalog.sh

# <script> <rpc port>: one script against its own rpc (+ worker for reservation) and catalog-rpc.
# Runs in a subshell: the pids, the traps and catalog.sh's APP_CATALOG_* exports stay its own.
run_one() {
  local s=$1 p=$2 env=()
  pid="" wpid=""   # not local: the EXIT trap runs after this function has returned (and this is a subshell)
  export APP_HTTP_ADDR=":$p"
  stop() {
    # worker first: SIGTERM drains in-flight handlers (≤ 8s) and exits 0. `|| true`: cleanup only; the verdict of a script is
    # k6's own exit status (the last command of run_one), never this.
    [ -n "$wpid" ] && kill "$wpid" 2>/dev/null && wait "$wpid" 2>/dev/null || true; wpid=""
    [ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null || true; pid=""
  }
  trap 'stop; catalog_stop' EXIT
  echo "== $s"
  catalog_start "$bin" "$((p + 2))" "$bin/k6-$s-catalog.log"
  case $s in
    ratelimit) env=(APP_HTTP_RATE_LIMIT_RPS=50 APP_HTTP_RATE_LIMIT_BURST=50) ;;
    timeout)   env=(APP_HTTP_TIMEOUT=1ns APP_LOG_LEVEL=error) ;;
  esac
  env ${env[@]+"${env[@]}"} "$bin/rpc" > "$bin/k6-$s.log" 2>&1 & pid=$!
  if [ "$s" = reservation ]; then
    # Defaults, not config.example.yaml (the rpc's env above would give it otel.endpoint
    # and the compose-up addresses it does not need). Same as the console e2e's worker.setup.ts.
    APP_CONFIG=/dev/null "$bin/worker" > "$bin/k6-$s-worker.log" 2>&1 & wpid=$!
  fi
  # up to 10s; bash arithmetic loop (no `seq`), a failing `sleep` is a failure of this script, and the end of the loop
  # is checked: ready, or this script fails (no external command's exit is ever read as "ready")
  local n ready=""
  for ((n = 0; n < 50; n++)); do
    kill -0 "$pid" 2>/dev/null || { echo "rpc exited:"; cat "$bin/k6-$s.log"; exit 1; }
    if curl -fsS "http://127.0.0.1:$p/api/readyz" >/dev/null 2>&1; then ready=1; break; fi
    sleep 0.2 || { echo "sleep failed while waiting for the rpc of $s"; exit 1; }
  done
  [ -n "$ready" ] || { echo "rpc of $s never became ready on :$p:"; cat "$bin/k6-$s.log"; exit 1; }
  # CREATE_P95: the smoke runs on a shared CI runner against a Postgres container, where
  # CreateOrder's p95 lands anywhere between 150 and 400ms at the same p50 as a laptop.
  # RESERVE_P95: same story for the worker chain (outbox poll + handler + the Postgres service).
  # RUN_ID per script: SKUs are named from it, and a worker reserving one script's SKUs
  # while another's setup() SetStocks them is an optimistic-lock conflict (concurrent_update).
  k6 run --quiet "tests/k6/$s.js" -e "BASE_URL=http://127.0.0.1:$p" -e VUS=10 -e DURATION=10s -e RPS=50 \
    -e "CREATE_P95=${CREATE_P95:-500}" -e "RESERVE_P95=${RESERVE_P95:-5000}" -e "RUN_ID=smoke-$$-$s"
}

# reservation alone first (empty outbox), then the rest together. Plain indexed arrays: macOS ships bash 3.2.
first=() firstp=() rest=() restp=()
for i in "${!scripts[@]}"; do
  if [ "${scripts[$i]}" = reservation ]; then first+=("${scripts[$i]}"); firstp+=($((port + 10 * i)))
  else rest+=("${scripts[$i]}"); restp+=($((port + 10 * i))); fi
done

for i in ${first[@]+"${!first[@]}"}; do
  s=${first[$i]}
  # every stage of the pipeline counts: the script's own status, and tee (a lost log is a failure too)
  # (both arms read PIPESTATUS before anything else runs; a bare `|| true` would overwrite it)
  # shellcheck disable=SC2015
  ( run_one "$s" "${firstp[$i]}" ) 2>&1 | tee "$bin/k6-$s.out" && st=("${PIPESTATUS[@]}") || st=("${PIPESTATUS[@]}")
  [ "${st[0]}" = 0 ] || exit 1
  [ "${st[1]}" = 0 ] || { echo "tee failed (exit ${st[1]}) while logging $s"; exit 1; }
done

if [ ${#rest[@]} -gt 0 ]; then
  pids=()
  for i in "${!rest[@]}"; do
    ( run_one "${rest[$i]}" "${restp[$i]}" ) > "$bin/k6-${rest[$i]}.out" 2>&1 & pids+=($!)
  done
  fail=0
  for i in "${!rest[@]}"; do
    rc=0; wait "${pids[$i]}" || rc=$?
    cat "$bin/k6-${rest[$i]}.out" || { echo "--- FAIL: ${rest[$i]} (its output could not be read)"; fail=1; }
    [ "$rc" = 0 ] || { echo "--- FAIL: ${rest[$i]} (exit $rc)"; fail=1; }
  done
  [ "$fail" = 0 ] || exit 1
fi
