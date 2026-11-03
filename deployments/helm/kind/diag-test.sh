#!/usr/bin/env bash
# What a failed kind run leaves behind (#416): diag.sh and its place in payment-flow.sh, against fake
# kubectl / helm / curl (no cluster). Cells, each with its own positive control:
#
#   D1  diag.sh, everything answers : the WHOLE log of every pod since the given time (no --tail), the previous
#                                     container's log, the four Postgres reads (sessions, blocked, locks, connections),
#                                     each read-only and bounded on the server
#   D2  diag.sh, parts fail         : a pod without a log, a failing Postgres read, no pod list: it still exits 0 and
#                                     still prints everything else (it must not hide the failure it reports on)
#   D3  payment-flow.sh, real entry : a step fails -> diag.sh ran ONCE, with the run's start time, BEFORE the release was
#                                     restored (helm upgrade replaces the pods and their logs), and the exit status is the
#                                     script's own (1), not diag's or restore's
#   D4  Postgres of the kind cluster: the server log settings that make a lock wait name its holder, and no parameters
#
# Every "output has X" is checked against text the fakes produce for THIS call (a nonce per run), so an empty or
# stale output cannot pass.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
WATCHDOG=""
trap 'test -z "$WATCHDOG" || kill $WATCHDOG 2>/dev/null; rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
nonce="n$$-$RANDOM"
since="2026-11-03T04:17:46Z"

# fake kubectl: MODE=ok answers everything; MODE=degraded fails the log of pod/a, the locks read and nothing else;
# MODE=nopods fails the pod list. Every call is appended to $EVENTS (shared with the fake helm, so order is visible).
cat > "$tmp/bin/kubectl" <<'K'
#!/usr/bin/env bash
echo "kubectl $*" >> "$EVENTS"
args="$*"
case $args in
  *port-forward*) exit 0 ;;
  *"get pods -o name"*)
    [ "${MODE:-ok}" = nopods ] && { echo "error: the server is currently unable to handle the request" >&2; exit 1; }
    printf 'pod/a\npod/b\n' ;;
  *"get pods -o wide"*) echo "PODS-$NONCE" ;;
  *"get events"*) echo "EVENTS-$NONCE" ;;
  *"get pods"*) echo "PODS-$NONCE" ;;
  *" logs "*)
    pod=$(sed -n 's/.* logs \([^ ]*\) .*/\1/p' <<<"$args")
    st=$(sed -n 's/.*--since-time=\([^ ]*\).*/\1/p' <<<"$args")
    if [[ $args == *--previous* ]]; then
      [ "$pod" = pod/b ] && echo "2026-11-03T04:18:00Z earlier-instance $NONCE"
      exit 0
    fi
    if [ "${MODE:-ok}" = degraded ] && [ "$pod" = pod/a ]; then echo "unable to retrieve container logs" >&2; exit 1; fi
    echo "2026-11-03T04:18:01Z LOG $pod since=$st $NONCE" ;;
  *"exec deploy/postgres"*)
    last=${args##*-c }
    if [ "${MODE:-ok}" = degraded ] && [[ $last == *pg_locks* ]]; then echo "psql: error: connection to server failed" >&2; exit 2; fi
    echo "SQL $NONCE: $last" ;;
  *) ;;
esac
K
cat > "$tmp/bin/helm" <<'H'
#!/usr/bin/env bash
echo "helm $*" >> "$EVENTS"
exit 0
H
# fake curl: the readiness probes answer; every API call is a 500 (so the first step of payment-flow.sh fails)
cat > "$tmp/bin/curl" <<'C'
#!/usr/bin/env bash
case "$*" in
  *readyz*) exit 0 ;;
  *) printf 'HTTP/1.1 500 Internal Server Error\r\n\r\n'; exit 0 ;;
esac
C
chmod +x "$tmp/bin/kubectl" "$tmp/bin/helm" "$tmp/bin/curl"

die() { echo "diag-test: $*" >&2; echo "---- output:" >&2; cat "$tmp/out" >&2 2>/dev/null || true; echo "---- events:" >&2; cat "$tmp/events" >&2 2>/dev/null || true; exit 1; }
has() { grep -qF -- "$1" "$tmp/out" || die "$2: output lacks '$1'"; }
hasnt() { ! grep -qF -- "$1" "$tmp/out" || die "$2: output has '$1'"; }

# run_bounded <seconds> <cmd...>: output in $tmp/out, exit status in $rc. A run that does not end in time is a failure.
# The watchdog polls once a second instead of sleeping for the whole limit, and its output goes nowhere: a
# long-lived `sleep` child would outlive the test holding its stdout (a caller's pipe stays open until it ends).
run_bounded() {
  local limit=$1 pid dog; shift
  : > "$tmp/events"
  EVENTS="$tmp/events" NONCE="$nonce" PATH="$tmp/bin:$PATH" "$@" > "$tmp/out" 2>&1 &
  pid=$!
  ( i=0; while [ "$i" -lt "$limit" ] && kill -0 "$pid" 2>/dev/null; do sleep 1; i=$((i + 1)); done
    if kill -0 "$pid" 2>/dev/null; then : > "$tmp/timeout"; kill -TERM "$pid" 2>/dev/null; fi ) >/dev/null 2>&1 &
  dog=$!
  WATCHDOG="$dog"
  rc=0; wait "$pid" || rc=$?
  kill "$dog" 2>/dev/null || true; wait "$dog" 2>/dev/null || true; WATCHDOG=""
  [ ! -e "$tmp/timeout" ] || die "$* did not end within ${limit}s"
}

# ── D1 diag.sh, everything answers ───────────────────────────────────────────
MODE=ok run_bounded 60 "$here/diag.sh" ns "$since"
[ "$rc" = 0 ] || die "D1: exit $rc, want 0"
for pod in pod/a pod/b; do
  has "LOG $pod since=$since $nonce" "D1 the log of $pod since the given time"
done
has "previous: 2026-11-03T04:18:00Z earlier-instance $nonce" "D1 the previous container's log"
has "PODS-$nonce" "D1 pods"; has "EVENTS-$nonce" "D1 events"
for want in pg_stat_activity pg_blocking_pids pg_locks max_connections; do
  grep -qF "$want" "$tmp/events" || die "D1: no Postgres read of $want"
  has "SQL $nonce:" "D1 the Postgres reads answered"
done
n=$(grep -c 'exec deploy/postgres' "$tmp/events" || true); [ "$n" = 4 ] || die "D1: $n Postgres reads, want 4 (sessions, blocked, locks, connections)"
grep -q -- '--tail' "$tmp/events" && die "D1: a log was cut with --tail"
grep -q "statement_timeout = '10s'" "$tmp/events" || die "D1: the Postgres reads are not bounded on the server"
grep -q 'default_transaction_read_only = on' "$tmp/events" || die "D1: the Postgres reads are not read-only"
grep -q -- '--request-timeout=30s' "$tmp/events" || die "D1: kubectl calls are not bounded"
# the text of a query is cut and carries no parameters; the SQL must not select pg_stat_activity.query whole
grep -q 'left(regexp_replace(query' "$tmp/events" || die "D1: the query text is not cut"
echo "diag-test ok: D1 the whole log since the start, the previous container, four bounded read-only Postgres reads"

# ── D2 diag.sh, parts fail ───────────────────────────────────────────────────
MODE=degraded run_bounded 60 "$here/diag.sh" ns "$since"
[ "$rc" = 0 ] || die "D2: exit $rc, want 0 (diag must not fail the run it reports on)"
has "diag: no log for pod/a" "D2 a pod without a log is named"
has "LOG pod/b since=$since $nonce" "D2 the other pod's log is still there"
has "diag: 'locks (pg_locks)' failed" "D2 the failing read is named"
has "SQL $nonce:" "D2 the other reads still ran"
n=$(grep -c 'exec deploy/postgres' "$tmp/events" || true); [ "$n" = 4 ] || die "D2: $n Postgres reads, want 4 (a failing one must not stop the rest)"
MODE=nopods run_bounded 60 "$here/diag.sh" ns "$since"
[ "$rc" = 0 ] || die "D2: no pod list: exit $rc, want 0"
has "diag: no pod list" "D2 no pod list is said, not silently empty"
has "SQL $nonce:" "D2 no pod list: Postgres reads still ran"
echo "diag-test ok: D2 a missing log, a failing read, no pod list: exit 0, the rest still printed"

# ── D3 payment-flow.sh through the real entry ────────────────────────────────
printf 'secrets:\n  apiKeys: "shopper=kind-shopper-key-0123456789ab,admin=kind-admin-key-0123456789abc:admin"\n' > "$tmp/flow.yaml"
: > "$tmp/values.yaml"
MODE=ok run_bounded 120 "$here/payment-flow.sh" ns rel chart "$tmp/values.yaml" "$tmp/flow.yaml"
[ "$rc" = 1 ] || die "D3: exit $rc, want 1 (the script's own status, not diag's or restore's)"
has "payment-flow: UpsertProduct: got '500', want '200'" "D3 the step that failed"
n=$(grep -c '^::group::diag: pods and events' "$tmp/out" || true); [ "$n" = 1 ] || die "D3: diag ran $n times, want exactly once"
start=$(sed -n 's/.*--since-time=\([^ ]*\).*/\1/p' "$tmp/events" | sort -u || true)
[ -n "$start" ] || die "D3: no log was read at all (diag.sh did not run)"
[ "$(wc -l <<<"$start" | tr -d ' ')" = 1 ] || die "D3: the logs were read since more than one time: $start"
[[ $start =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || die "D3: since-time '$start' is not RFC 3339 UTC"
has "LOG pod/a since=$start $nonce" "D3 the pod log reached the output"
first_logs=$(grep -n ' logs ' "$tmp/events" | head -1 | cut -d: -f1 || true)
last_helm=$(grep -n '^helm upgrade' "$tmp/events" | tail -1 | cut -d: -f1 || true)
first_helm=$(grep -n '^helm upgrade' "$tmp/events" | head -1 | cut -d: -f1 || true)
[ -n "$first_logs" ] && [ -n "$last_helm" ] || die "D3: no log read or no restore in the events"
[ "$first_helm" -lt "$first_logs" ] || die "D3: the flow's own helm upgrade did not come first (the fake run did not get as far as it should)"
[ "$first_logs" -lt "$last_helm" ] || die "D3: the pods' logs were read AFTER the release was restored (helm upgrade replaced them)"
echo "diag-test ok: D3 a failing step: diag ran once, since $start, before the release was restored; exit 1"

# ── D4 the kind Postgres writes the server log a lock wait needs ─────────────
# PyYAML is already what deployments/helm/check.py needs, so this adds no tool
args=$(python3 - "$here/deps.yaml" <<'PY' || true
import sys, yaml
for doc in yaml.safe_load_all(open(sys.argv[1])):
    if doc and doc.get("kind") == "Deployment" and doc["metadata"]["name"] == "postgres":
        print(" ".join(doc["spec"]["template"]["spec"]["containers"][0].get("args", [])))
PY
)
[ -n "$args" ] || die "D4: the postgres container of deps.yaml has no args"
for want in "log_lock_waits=on" "deadlock_timeout=1s" "log_min_duration_statement=500" "log_parameter_max_length=0" "log_parameter_max_length_on_error=0"; do
  [[ " $args " == *" $want "* ]] || die "D4: postgres args lack $want (have: $args)"
done
echo "diag-test ok: D4 postgres logs lock waits (1s), statements over 500ms, and no parameters"
echo "diag-test ok"
