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
# A run only counts if the broken answer was really served (the server's own `served <mode>` line)
# and k6 reports exactly the failure class that mode must produce — a server that is absent or
# serves the wrong thing fails the cell (a refused connection also fails k6 and leaks nothing, which
# is why neither exit code nor absence of the key proves anything). The last two cells
# are negatives: no server at all, and a server in the wrong mode — wire.sh itself must go red.
#   WIRE_SERVER_CMD=<cmd>  replaces `python3 tests/k6/wire_server.py` (the negatives use it)
#   WIRE_SERVE_AS=<mode>   makes the server serve that mode whatever the cell asks for
#   WIRE_MODES="a b"       only these modes;   WIRE_NO_NEGATIVES=1  skip the two negative cells
# Failures are printed as `--- FAIL: <cell>` + indented reasons, the shape scripts/red-stage.sh reads.
# The key is made up for this test; nothing here is a credential.
set -euo pipefail
bin=$1; shift
scripts=("$@"); [ ${#scripts[@]} -gt 0 ] || scripts=(orders ratelimit timeout reservation)
port=${PORT:-18190}
KEY=k6-fake-wire-canary-0001
# setup()'s SetStock sends ADMIN_API_KEY, a second key the server reflects: both must stay out
ADMIN_KEY=k6-fake-wire-admin-canary-0002
dir="$bin/k6-wire"; mkdir -p "$dir"
server_cmd=${WIRE_SERVER_CMD:-python3 tests/k6/wire_server.py}
# the class k6 must report for each mode (rpc() in lib.js: transport.<class>), and no other
want() {
  case $1 in
    status|version|header|chunk) echo malformed_response ;;
    truncated) echo truncated_response ;;
    closed) echo connection_closed ;;
  esac
}

xpid=""
stop() { [ -n "$xpid" ] && kill "$xpid" 2>/dev/null && wait "$xpid" 2>/dev/null || true; xpid=""; }
trap stop EXIT

fails=()
for s in "${scripts[@]}"; do
  for mode in ${WIRE_MODES:-status version header chunk truncated closed}; do
    cell="wire-$s-$mode" log="$dir/$s-$mode" t0=$SECONDS
    echo "=== RUN   $cell"
    why=()
    $server_cmd "$port" "${WIRE_SERVE_AS:-$mode}" > "$log-server.log" 2>&1 & xpid=$!
    up=""
    for _ in $(seq 1 50); do
      (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && { up=1; break; }
      sleep 0.1
    done
    [ -n "$up" ] || why+=("wire server did not listen on :$port within 5s")
    rc=0
    k6 run --quiet --summary-export "$log-summary.json" --out "json=$log-metrics.json" \
      "tests/k6/$s.js" -e "BASE_URL=http://127.0.0.1:$port" -e "API_KEY=$KEY" -e "ADMIN_API_KEY=$ADMIN_KEY" -e VUS=1 -e DURATION=2s \
      -e RPS=5 -e "RUN_ID=wire-$$-$s-$mode" > "$log.log" 2>&1 || rc=$?
    # alive at the end: a server that died mid-run answered nothing after that
    kill -0 "$xpid" 2>/dev/null || why+=("wire server exited during the run")
    stop
    grep -q "^served ${mode}$" "$log-server.log" 2>/dev/null \
      || why+=("wire server never served '$mode' (its log has no 'served $mode' line)")
    [ "$rc" != 0 ] || why+=("k6 exited 0: a transport failure must fail the run")
    for f in "$log.log" "$log-summary.json" "$log-metrics.json"; do
      for k in "$KEY" "$ADMIN_KEY"; do
        if grep -q -- "$k" "$f" 2>/dev/null; then
          why+=("forbidden text $k found in $(basename "$f") (a credential reflected by the wire parser error)")
        fi
      done
    done
    # nothing free-text in a tag: k6's `error` tag is the parser's own message
    if grep -q '"error":"' "$log-metrics.json" 2>/dev/null; then
      why+=("metrics carry a free-text error tag (the parser message)")
    fi
    # the diagnostic: exactly the class this mode produces, and no other
    classes=$(grep -o 'transport\.[a-z_]*' "$log.log" | sort -u | tr '\n' ' ' || true)
    if [ "$classes" != "transport.$(want "$mode") " ]; then
      why+=("expected the only failure class transport.$(want "$mode"), output has: ${classes:-none}")
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

# a key that IS a failure class string (a well-formed made-up key, `transport.<class>`): rpc() decides
# on the class but must print it through redact() like any other output, so the key never shows. The
# status wire answer reflects it as the status code; both keys are set to the same string, so the
# SetStock of setup() (ADMIN_API_KEY) and every other call (API_KEY) are covered.
if [ -z "${WIRE_NO_NEGATIVES:-}" ]; then
  for s in orders timeout; do
    for c in malformed_response truncated_response timeout connection_refused dns connection_closed other; do
      case " ${scripts[*]} " in *" $s "*) ;; *) continue ;; esac
      cell="wire-$s-key-is-transport.$c" log="$dir/$s-key-$c" k="transport.$c"
      echo "=== RUN   $cell"
      why=()
      python3 tests/k6/wire_server.py "$port" status > "$log-server.log" 2>&1 & xpid=$!
      for _ in $(seq 1 50); do (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && break; sleep 0.1; done
      rc=0
      k6 run --quiet --summary-export "$log-summary.json" --out "json=$log-metrics.json" \
        "tests/k6/$s.js" -e "BASE_URL=http://127.0.0.1:$port" -e "API_KEY=$k" -e "ADMIN_API_KEY=$k" -e VUS=1 \
        -e DURATION=2s -e RPS=5 -e "RUN_ID=wire-$$-$s-key-$c" > "$log.log" 2>&1 || rc=$?
      stop
      grep -q "^served status$" "$log-server.log" 2>/dev/null || why+=("wire server never served 'status'")
      [ "$rc" != 0 ] || why+=("k6 exited 0: a transport failure must fail the run")
      for f in "$log.log" "$log-summary.json" "$log-metrics.json"; do
        if grep -q -- "$k" "$f" 2>/dev/null; then
          why+=("forbidden text $k found in $(basename "$f") (a key equal to a failure class string)")
        fi
      done
      # the status wire answer is a malformed_response: shown as is, or redacted when the key is that very string
      if [ "$c" = malformed_response ]; then want_out='<redacted>'; else want_out='transport.malformed_response'; fi
      grep -q -- "$want_out" "$log.log" || why+=("no diagnostic: output lacks $want_out")
      if [ ${#why[@]} -gt 0 ]; then
        echo "--- FAIL: $cell (k6 exit $rc)"
        printf '    %s\n' "${why[@]}"
        fails+=("$cell")
      else
        echo "--- PASS: $cell (k6 exit $rc)"
      fi
    done
  done
fi

# legal HTTP, legal JSON, but an enum field quotes the key back (tests/k6/status_server.py): the
# diagnostic tags of reservation.js (reservation_wrong_status / order_not_confirmed /
# reservation_not_committed) are built from response fields, so the key must not reach them. Each
# run must still fail its own threshold (a status that is not the expected one is counted as wrong,
# redacting must not turn it into a pass), and the tag must be the fixed bucket. The last cell sets
# the key to a legal enum value: the value passes the allowlist, so only redact() keeps it out.
case " ${scripts[*]} " in *" reservation "*)
  for cell_def in "reservation:reservation_wrong_status:$KEY:unrecognized" \
                  "order:order_not_confirmed:$KEY:unrecognized" \
                  "commit:reservation_not_committed:$KEY:unrecognized" \
                  "reservation:reservation_wrong_status:RESERVATION_STATUS_EXPIRED:redacted"; do
    IFS=: read -r mode counter k want_tag <<<"$cell_def"
    cell="wire-reservation-reflect-$mode-$want_tag" log="$dir/reservation-reflect-$mode-$want_tag"
    echo "=== RUN   $cell"
    why=()
    python3 tests/k6/status_server.py "$port" "$mode" > "$log-server.log" 2>&1 & xpid=$!
    for _ in $(seq 1 50); do (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && break; sleep 0.1; done
    rc=0
    k6 run --quiet --summary-export "$log-summary.json" --out "json=$log-metrics.json" \
      tests/k6/reservation.js -e "BASE_URL=http://127.0.0.1:$port" -e "API_KEY=$k" -e "ADMIN_API_KEY=$k" -e VUS=1 \
      -e DURATION=2s -e SKUS=1 -e "RUN_ID=wire-$$-reflect-$mode" > "$log.log" 2>&1 || rc=$?
    stop
    grep -q "^served $mode$" "$log-server.log" 2>/dev/null || why+=("status server never served the reflected '$mode' answer")
    [ "$rc" != 0 ] || why+=("k6 exited 0: a wrong status must fail $counter")
    for f in "$log.log" "$log-summary.json" "$log-metrics.json"; do
      if grep -q -- "$k" "$f" 2>/dev/null; then
        why+=("forbidden text $k found in $(basename "$f") (a credential reflected into a legal JSON status)")
      fi
    done
    # the counter was really hit, with the fixed bucket as its tag
    grep -q "\"metric\":\"$counter\"" "$log-metrics.json" 2>/dev/null \
      || why+=("no $counter sample: the threshold decision changed or the fixture was not reached")
    grep -q "\"status\":\"[^\"]*$want_tag" "$log-metrics.json" 2>/dev/null \
      || why+=("$counter carries no status tag with $want_tag")
    if [ ${#why[@]} -gt 0 ]; then
      echo "--- FAIL: $cell (k6 exit $rc)"
      printf '    %s\n' "${why[@]}"
      fails+=("$cell")
    else
      echo "--- PASS: $cell (k6 exit $rc)"
    fi
  done ;;
esac

# negatives: the gate must go red when its fixture is absent or wrong
if [ -z "${WIRE_NO_NEGATIVES:-}" ] && [ -z "${WIRE_SERVER_CMD:-}${WIRE_SERVE_AS:-}" ]; then
  for neg in "absent:WIRE_SERVER_CMD=false" "wrong-mode:WIRE_SERVE_AS=closed"; do
    echo "=== RUN   wire-negative-${neg%%:*}"
    if env WIRE_NO_NEGATIVES=1 WIRE_MODES=status PORT=$((port + 1)) "${neg#*:}" "$0" "$bin" orders > "$dir/negative-${neg%%:*}.log" 2>&1; then
      echo "--- FAIL: wire-negative-${neg%%:*}"
      echo "    wire.sh stayed green with the fixture ${neg%%:*}: the gate proves nothing"
      fails+=("wire-negative-${neg%%:*}")
    else
      echo "--- PASS: wire-negative-${neg%%:*} (red as it must)"
    fi
  done
fi

if [ ${#fails[@]} -gt 0 ]; then echo "FAIL"; echo "wire gate FAILED: ${fails[*]}"; exit 1; fi
echo "ok  wire gate"
