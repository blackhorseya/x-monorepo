#!/usr/bin/env bash
# The k6 wire gate: the four scripts against a server whose HTTP response is broken (non-HTTP
# status line, bad version, header without a colon, bad chunk, truncated body, closed
# connection), the first four quoting the request's X-API-Key back as a misbehaving upstream or
# proxy would (tests/k6/wire_server.py). Go's HTTP parser puts what it could not parse into its
# error, and k6 prints that error ("Request Failed" warning) and tags every sample with it before any
# script code runs — auth-matrix.sh's fixtures send legal HTTP and cannot see this.
# Each run must fail (a transport failure is a failed request) and the key must be in none of: k6's
# output, --summary-export, --out json (every metric tag); the connection closed without a byte
# (no key on the wire) must still leave a diagnostic that names a class of failure.
#   wire.sh <bin dir> [script…]      (default: orders ratelimit timeout reservation)
# Failures are printed as `--- FAIL: <cell>` + indented reasons, the shape scripts/red-stage.sh reads.
# The key is made up for this test; nothing here is a credential.
set -euo pipefail
bin=$1; shift
scripts=("$@"); [ ${#scripts[@]} -gt 0 ] || scripts=(orders ratelimit timeout reservation)
port=${PORT:-18190}
KEY=k6-fake-wire-canary-0001
dir="$bin/k6-wire"; mkdir -p "$dir"

xpid=""
stop() { [ -n "$xpid" ] && kill "$xpid" 2>/dev/null && wait "$xpid" 2>/dev/null || true; xpid=""; }
trap stop EXIT

fails=()
for s in "${scripts[@]}"; do
  for mode in status version header chunk truncated closed; do
    cell="wire-$s-$mode" log="$dir/$s-$mode" t0=$SECONDS
    echo "=== RUN   $cell"
    python3 tests/k6/wire_server.py "$port" "$mode" > "$log-server.log" 2>&1 & xpid=$!
    for _ in $(seq 1 50); do (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && break; sleep 0.1; done
    rc=0
    k6 run --quiet --summary-export "$log-summary.json" --out "json=$log-metrics.json" \
      "tests/k6/$s.js" -e "BASE_URL=http://127.0.0.1:$port" -e "API_KEY=$KEY" -e VUS=1 -e DURATION=2s \
      -e RPS=5 -e "RUN_ID=wire-$$-$s-$mode" > "$log.log" 2>&1 || rc=$?
    stop
    why=()
    [ "$rc" != 0 ] || why+=("k6 exited 0: a transport failure must fail the run")
    for f in "$log.log" "$log-summary.json" "$log-metrics.json"; do
      if grep -q -- "$KEY" "$f" 2>/dev/null; then
        why+=("forbidden text $KEY found in $(basename "$f") (a credential reflected by the wire parser error)")
      fi
    done
    # nothing free-text in a tag: k6's `error` tag is the parser's own message
    if grep -q '"error":"' "$log-metrics.json" 2>/dev/null; then
      why+=("metrics carry a free-text error tag (the parser message)")
    fi
    if [ "$mode" = closed ] && ! grep -q 'transport\.' "$log.log"; then
      why+=("no safe diagnostic: output lacks a transport.<class> failure")
    fi
    if [ ${#why[@]} -gt 0 ]; then
      echo "--- FAIL: $cell (k6 exit $rc)"
      printf '    %s\n' "${why[@]}"
      fails+=("$cell")
    else
      echo "--- PASS: $cell (k6 exit $rc, $((SECONDS - t0))s)"
    fi
  done
done

if [ ${#fails[@]} -gt 0 ]; then echo "FAIL"; echo "wire gate FAILED: ${fails[*]}"; exit 1; fi
echo "ok  wire gate"
