#!/usr/bin/env bash
# The k6 key gate: ratelimit.js and timeout.js against a real rpc + Postgres, with the API key set to a made-up value
# that must never reach k6's output, summary or metrics — also when the server answers with a 503 or a 307 that
# echoes the key (fault_proxy.py), or when the key is itself one of the codes a script decides on (#209, #193).
# Why: a green run on a plain server says nothing about what k6 prints when the answer is not the one it expects.
#   auth-matrix.sh <bin dir> [cell…]      (default: all; cell names are the first word of each cell() call)
# A named cell that does not exist is an error (a typo would otherwise run nothing and stay green).
# Each cell has its own rpc and log; the scripts' own thresholds decide pass/fail, and a cell states the k6
# exit code it must end with. The key below is made up for this test; nothing here is a credential.
set -euo pipefail
bin=$1; shift
only=("$@")
port=${PORT:-18090}
export APP_CONFIG=configs/config.example.yaml APP_HTTP_ADDR=":$port" APP_HTTP_DOCS=false
export APP_POSTGRES_URL=${DATABASE_URL:-postgres://app:app@127.0.0.1:5432/app?sslmode=disable}
export APP_LOG_LEVEL=${APP_LOG_LEVEL:-warn}

PLAIN_KEY=k6-fake-roleless-key-0001
LIST=/identity.v1.IdentityService/ListAccounts

pid="" xpid=""
stop() {
  [ -n "$xpid" ] && kill "$xpid" 2>/dev/null && wait "$xpid" 2>/dev/null || true; xpid=""
  [ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null || true; pid=""
}
trap stop EXIT

fails=() seen=()
# cell <name> <script> <expect: the k6 exit code> <match: text the k6 output must contain, or -> <key|-> <server env…>
#   env knobs: FAIL_PATH=<procedure> → a fault proxy in front of the rpc answers that procedure's
#   first call 503 (once), REFLECT=1|header|json|prefixed|redirect|redirect-path → echoing the request's X-API-Key in it; K6_ENV="a=b c=d" → extra
#   k6 -e; FORBID=<text> → the k6 output, summary export and metrics stream must not contain it (a key must never be printed).
cell() {
  local name=$1 s=$2 expect=$3 match=$4 key=$5; shift 5
  seen+=("$name")
  if [ ${#only[@]} -gt 0 ]; then
    local hit=""; for o in "${only[@]}"; do [ "$o" = "$name" ] && hit=1; done
    [ -n "$hit" ] || return 0
  fi
  local log="$bin/k6-auth-$name" id="auth-$$-$name" target="http://127.0.0.1:$port" t0=$SECONDS
  echo "== $name ($s, expect exit $expect)"
  env "$@" "$bin/rpc" > "$log-rpc.log" 2>&1 & pid=$!
  for ((_try54 = 1; _try54 <= 50; _try54++)); do
    kill -0 "$pid" 2>/dev/null || { echo "rpc exited:"; cat "$log-rpc.log"; exit 1; }
    curl -fsS "http://127.0.0.1:$port/api/readyz" >/dev/null 2>&1 && break
    sleep 0.2
  done
  if [ -n "${FAIL_PATH:-}" ]; then
    python3 tests/k6/fault_proxy.py "$((port + 1))" "$port" "$FAIL_PATH" > "$log-proxy.log" 2>&1 & xpid=$!
    target="http://127.0.0.1:$((port + 1))"; sleep 0.5
  fi
  local kenv=(-e "BASE_URL=$target" -e VUS=2 -e DURATION=5s -e RPS=50 -e "RUN_ID=$id")
  [ "$key" = - ] || kenv+=(-e "API_KEY=$key")
  for kv in ${K6_ENV:-}; do kenv+=(-e "$kv"); done
  local rc=0
  k6 run --quiet --summary-export "$log-summary.json" --out "json=$log-metrics.json" \
    "tests/k6/$s.js" "${kenv[@]}" > "$log.log" 2>&1 || rc=$?
  stop
  if [ "$rc" != "$expect" ]; then
    echo "   ✗ $name: expected k6 exit $expect, got $rc"; tail -n 25 "$log.log"; fails+=("$name")
  elif [ -n "${FORBID:-}" ] && grep -q -- "$FORBID" "$log.log" "$log-summary.json" "$log-metrics.json" 2>/dev/null; then
    echo "   ✗ $name: k6 output / summary / metrics contain the forbidden text (a credential?)"; fails+=("$name")
  elif [ "$match" != - ] && ! grep -q -- "$match" "$log.log"; then
    echo "   ✗ $name: k6 output lacks '$match'"; tail -n 25 "$log.log"; fails+=("$name")
  else
    echo "   ✓ $name (k6 exit $rc, $((SECONDS - t0))s)"
  fi
}

# ── the cells ──
# A key that is sent and a rate limit / a deadline that decide: the scripts must pass whatever the key is.
RL=(APP_AUTH_MODE=off APP_HTTP_RATE_LIMIT_RPS=50 APP_HTTP_RATE_LIMIT_BURST=50)
TO=(APP_AUTH_MODE=off APP_HTTP_TIMEOUT=1ns APP_LOG_LEVEL=error)
cell ratelimit-key ratelimit 0 - "$PLAIN_KEY" "${RL[@]}"
cell ratelimit-key-is-code ratelimit 0 - request.rate_limited "${RL[@]}"
cell timeout-key timeout 0 - "$PLAIN_KEY" "${TO[@]}"
cell timeout-key-is-code timeout 0 - request.timeout "${TO[@]}"

# The first ListAccounts answers 503 (once), as a body or a header that echoes the key (REFLECT): the run fails
# its thresholds (99) and the key must be in none of k6's output, summary export or metrics stream.
for m in 1 header json prefixed; do
  FAIL_PATH=$LIST REFLECT=$m FORBID=$PLAIN_KEY cell "ratelimit-503-reflect-$m" ratelimit 99 - "$PLAIN_KEY" "${RL[@]}"
done
# A 307 whose Location carries the key: not followed, not logged, not tagged.
FAIL_PATH=$LIST REFLECT=redirect FORBID=$PLAIN_KEY cell ratelimit-307 ratelimit 99 - "$PLAIN_KEY" "${RL[@]}"
FAIL_PATH=$LIST REFLECT=redirect-path FORBID=$PLAIN_KEY cell ratelimit-307-path ratelimit 99 - "$PLAIN_KEY" "${RL[@]}"
# A key that is itself a known error code, echoed as the code: redacted in the metric tags too.
CODE_KEY=request.validation_failed
FAIL_PATH=$LIST REFLECT=json FORBID=$CODE_KEY cell ratelimit-key-is-code-reflected ratelimit 99 - "$CODE_KEY" "${RL[@]}"

# every named cell must exist: a typo would otherwise run nothing and stay green
for o in ${only[@]+"${only[@]}"}; do
  hit=""; for n in "${seen[@]}"; do [ "$n" = "$o" ] && hit=1; done
  [ -n "$hit" ] || { echo "auth matrix FAILED: no such cell '$o' (cells: ${seen[*]})" >&2; exit 1; }
done
if [ ${#fails[@]} -gt 0 ]; then echo "auth matrix FAILED: ${fails[*]}"; exit 1; fi
echo "auth matrix ok"
