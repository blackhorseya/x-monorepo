#!/usr/bin/env bash
# Runs each k6 script against its own ./bin/rpc (built already) with the Postgres at $DATABASE_URL:
# ratelimit with the server at 50 rps, timeout with the server at a 1ns deadline. `task k6-smoke` locally,
# the `k6` job in CI.
#   smoke.sh <bin dir> [script…]        (default: ratelimit timeout)
#
# The scripts run at the same time (#451). What each shares with the others, and how it is kept apart:
#   Postgres   shared (one database). ratelimit reads, timeout gets a 504 before touching rows.
#   rpc port   $PORT + 10 × (position in the list).
#   logs       $bin/k6-<script>*.log; k6 output is buffered per script and printed in list order.
set -euo pipefail
bin=$1; shift
scripts=("$@"); [ ${#scripts[@]} -gt 0 ] || scripts=(ratelimit timeout)
port=${PORT:-18090}
export APP_CONFIG=configs/config.example.yaml APP_HTTP_ADDR=":$port" APP_HTTP_DOCS=false
export APP_POSTGRES_URL=${DATABASE_URL:-postgres://app:app@127.0.0.1:5432/app?sslmode=disable}
export APP_LOG_LEVEL=${APP_LOG_LEVEL:-warn}

# <script> <rpc port>: one script against its own rpc.
# Runs in a subshell: the pids and the traps stay its own.
run_one() {
  local s=$1 p=$2 env=()
  pid=""   # not local: the EXIT trap runs after this function has returned (and this is a subshell)
  export APP_HTTP_ADDR=":$p"
  stop() {
    # `|| true`: cleanup only; the verdict of a script is k6's own exit status (the last command of run_one), never this.
    [ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null || true; pid=""
  }
  trap stop EXIT
  echo "== $s"
  case $s in
    ratelimit) env=(APP_HTTP_RATE_LIMIT_RPS=50 APP_HTTP_RATE_LIMIT_BURST=50) ;;
    timeout)   env=(APP_HTTP_TIMEOUT=1ns APP_LOG_LEVEL=error) ;;
  esac
  env ${env[@]+"${env[@]}"} "$bin/rpc" > "$bin/k6-$s.log" 2>&1 & pid=$!
  # up to 10s; bash arithmetic loop (no `seq`), a failing `sleep` is a failure of this script, and the end of the loop
  # is checked: ready, or this script fails (no external command's exit is ever read as "ready")
  local n ready=""
  for ((n = 0; n < 50; n++)); do
    kill -0 "$pid" 2>/dev/null || { echo "rpc exited:"; cat "$bin/k6-$s.log"; exit 1; }
    if curl -fsS "http://127.0.0.1:$p/api/readyz" >/dev/null 2>&1; then ready=1; break; fi
    sleep 0.2 || { echo "sleep failed while waiting for the rpc of $s"; exit 1; }
  done
  [ -n "$ready" ] || { echo "rpc of $s never became ready on :$p:"; cat "$bin/k6-$s.log"; exit 1; }
  k6 run --quiet "tests/k6/$s.js" -e "BASE_URL=http://127.0.0.1:$p" -e VUS=10 -e DURATION=10s -e RPS=50
}

rest=() restp=()
for i in "${!scripts[@]}"; do
  rest+=("${scripts[$i]}"); restp+=($((port + 10 * i)))
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
