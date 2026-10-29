#!/usr/bin/env bash
# The k6 auth gate: orders.js and reservation.js (small VU, functional) against a real rpc +
# worker (in every cell) + Postgres, once per auth state — 8 cells — plus the fixtures that pin what the
# scripts do in the awkward ones (per-procedure public, a procedure that fails once).
# Why: a green run on the default auth-off server says nothing about a role-less key, an
# admin key, or a key sent to a server that ignores it; #187 found each by hand (#193).
#   auth-matrix.sh <bin dir> [cell…]      (default: all; cell names are the first word of each cell() call)
# Each cell has its own rpc (and worker for reservation), RUN_ID (→ SKUs and customers) and log;
# the scripts' own thresholds decide pass/fail (rpc_errors, p95s, and now: the operations
# that must have succeeded are > 0), unchanged and with the same CREATE_P95 / RESERVE_P95 as
# smoke.sh. The keys below are made up for this test; nothing here is a credential.
set -euo pipefail
bin=$1; shift
only=("$@")
port=${PORT:-18090}
export APP_CONFIG=configs/config.example.yaml APP_HTTP_ADDR=":$port" APP_HTTP_DOCS=false
export APP_POSTGRES_URL=${DATABASE_URL:-postgres://app:app@127.0.0.1:5432/app?sslmode=disable}
export APP_LOG_LEVEL=${APP_LOG_LEVEL:-warn}

PLAIN_KEY=k6-fake-roleless-key-0001
ADMIN_KEY=k6-fake-admin-key-000002
WHOAMI=/identity.v1.IdentityService/WhoAmI
CREATE=/ordering.v1.OrderService/CreateOrder

pid="" wpid="" xpid=""
stop() {
  [ -n "$xpid" ] && kill "$xpid" 2>/dev/null && wait "$xpid" 2>/dev/null || true; xpid=""
  [ -n "$wpid" ] && kill "$wpid" 2>/dev/null && wait "$wpid" 2>/dev/null || true; wpid=""
  [ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null || true; pid=""
}
trap stop EXIT

fails=()
# cell <name> <script> <expect: the k6 exit code> <match: text the k6 output must contain, or -> <key|-> <server env…>
#   env knobs: FAIL_PATH=<procedure> → a fault proxy in front of the rpc answers that procedure's
#   first call 503 (once), REFLECT=1 → echoing the request's X-API-Key in it; K6_ENV="a=b c=d" → extra
#   k6 -e; FORBID=<text> → the k6 output must not contain it (a key must never be printed).
cell() {
  local name=$1 s=$2 expect=$3 match=$4 key=$5; shift 5
  if [ ${#only[@]} -gt 0 ]; then
    local hit=""; for o in "${only[@]}"; do [ "$o" = "$name" ] && hit=1; done
    [ -n "$hit" ] || return 0
  fi
  local log="$bin/k6-auth-$name" id="auth-$$-$name" target="http://127.0.0.1:$port" t0=$SECONDS
  echo "== $name ($s, expect exit $expect)"
  env "$@" "$bin/rpc" > "$log-rpc.log" 2>&1 & pid=$!
  # a worker in every cell: orders cells leave their outbox for it too, so the next cell's worker
  # does not start on a backlog (that is what stretches reservation_latency on a reused database)
  APP_CONFIG=/dev/null "$bin/worker" > "$log-worker.log" 2>&1 & wpid=$!
  for _ in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || { echo "rpc exited:"; cat "$log-rpc.log"; exit 1; }
    curl -fsS "http://127.0.0.1:$port/api/readyz" >/dev/null 2>&1 && break
    sleep 0.2
  done
  if [ -n "${FAIL_PATH:-}" ]; then
    python3 tests/k6/fault_proxy.py "$((port + 1))" "$port" "$FAIL_PATH" > "$log-proxy.log" 2>&1 & xpid=$!
    target="http://127.0.0.1:$((port + 1))"; sleep 0.5
  fi
  local kenv=(-e "BASE_URL=$target" -e VUS=2 -e DURATION=5s -e RPS=50
    -e "CREATE_P95=${CREATE_P95:-500}" -e "RESERVE_P95=${RESERVE_P95:-5000}" -e "RUN_ID=$id")
  [ "$key" = - ] || kenv+=(-e "API_KEY=$key")
  for kv in ${K6_ENV:-}; do kenv+=(-e "$kv"); done
  local rc=0
  k6 run --quiet "tests/k6/$s.js" "${kenv[@]}" > "$log.log" 2>&1 || rc=$?
  stop
  if [ "$rc" != "$expect" ]; then
    echo "   ✗ $name: expected k6 exit $expect, got $rc"; tail -n 25 "$log.log"; fails+=("$name")
  elif [ -n "${FORBID:-}" ] && grep -q -- "$FORBID" "$log.log"; then
    echo "   ✗ $name: k6 output contains the forbidden text (a credential?)"; fails+=("$name")
  elif [ "$match" != - ] && ! grep -q -- "$match" "$log.log"; then
    echo "   ✗ $name: k6 output lacks '$match'"; tail -n 25 "$log.log"; fails+=("$name")
  else
    echo "   ✓ $name (k6 exit $rc, $((SECONDS - t0))s)"
  fi
}

# ── the 8 cells: {auth on, role-less key | auth on, admin key | auth off, no key | auth off, key sent} × script ──
ON=(APP_AUTH_MODE=required)
for s in reservation orders; do
  cell "on-roleless-$s" "$s" 0 - "$PLAIN_KEY" "${ON[@]}" "APP_AUTH_API_KEYS=k6-plain=$PLAIN_KEY"
  cell "on-admin-$s"    "$s" 0 - "$ADMIN_KEY" "${ON[@]}" "APP_AUTH_API_KEYS=k6-admin=$ADMIN_KEY:admin"
  cell "off-nokey-$s"   "$s" 0 - -            APP_AUTH_MODE=off
  cell "off-key-$s"     "$s" 0 - "$PLAIN_KEY" APP_AUTH_MODE=off "APP_AUTH_API_KEYS=k6-plain=$PLAIN_KEY"
done

# ── fixtures: per-procedure public, and a call that fails once ──
# Only WhoAmI public: WhoAmI answers anonymous for everyone, but the key still authenticates every
# other procedure — the script must not read "anonymous" off WhoAmI.
cell whoami-public orders 0 - "$PLAIN_KEY" "${ON[@]}" "APP_AUTH_API_KEYS=k6-plain=$PLAIN_KEY" "APP_AUTH_PUBLIC=$WHOAMI"
# Only CreateOrder public with a role-less key: CreateOrder is anonymous (needs a customer), then
# the key's GetOrder cannot see that customer's order. Not a supported combination (docs/operations.md):
# it must fail (99: thresholds crossed), and with the server's real answer in the output.
cell create-public orders 99 order.not_found "$PLAIN_KEY" "${ON[@]}" "APP_AUTH_API_KEYS=k6-plain=$PLAIN_KEY" "APP_AUTH_PUBLIC=$CREATE"
# WhoAmI fails once (503): nothing may be decided from it, so the run is unaffected.
FAIL_PATH=$WHOAMI cell whoami-503 orders 0 - "$PLAIN_KEY" "${ON[@]}" "APP_AUTH_API_KEYS=k6-plain=$PLAIN_KEY"
# The deciding CreateOrder fails once (503): never cached as an answer, aborts (108) naming the source.
FAIL_PATH=$CREATE cell create-503 orders 108 "customer probe" "$PLAIN_KEY" "${ON[@]}" "APP_AUTH_API_KEYS=k6-plain=$PLAIN_KEY"

# The same failure with a body that echoes the key: abort names RPC, status and code, never the body.
FAIL_PATH=$CREATE REFLECT=1 FORBID=$PLAIN_KEY cell create-503-reflect orders 108 "customer probe" "$PLAIN_KEY" "${ON[@]}" "APP_AUTH_API_KEYS=k6-plain=$PLAIN_KEY"

if [ ${#fails[@]} -gt 0 ]; then echo "auth matrix FAILED: ${fails[*]}"; exit 1; fi
echo "auth matrix ok"
