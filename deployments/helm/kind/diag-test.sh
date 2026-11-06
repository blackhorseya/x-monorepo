#!/usr/bin/env bash
# What a failed kind run leaves behind (#416): diag.sh and its place in payment-flow.sh, against fake
# kubectl / helm / curl (no cluster). Cells, each with its own positive control:
#
#   D1  diag.sh, everything answers : the WHOLE log of every pod since the given time as files (no --tail), the previous
#                                     container's log, the three Postgres reads (sessions with blocked_by, locks,
#                                     connections), each read-only, bounded on the server, with no query text
#   D2  diag.sh, parts fail         : a pod without a log, a failing Postgres read, no pod list: it still exits 0 and
#                                     still prints everything else (it must not hide the failure it reports on)
#   D3  payment-flow.sh, real entry : a step fails -> diag.sh ran ONCE, with the run's start time, BEFORE the release was
#                                     restored (helm upgrade replaces the pods and their logs), and the exit status is the
#                                     script's own (1), not diag's or restore's
#   D4  Postgres of the kind cluster: the server log settings that make a lock wait name its holder and keep SQL text out
#   D5  signals, real entry         : TERM and INT while a helm upgrade waits -> diag ran, BEFORE the restore, exit 128+signal,
#                                     no child of the script left (#420)
#   D6  bounds                      : many pods x many lines -> the Actions console stays small, the files are complete, and a
#                                     slow cluster ends at the deadline (#420)
#   D6L/W/E/S/M the console in BYTES  : one 1 MiB line per pod / 5000 pod rows / huge event lines / 100000 Postgres rows: the
#                                     console stays under its byte budget, says it was cut and where the files are, and the
#                                     files are complete (a limit in lines is no limit: #417 r2)
#   D6X        two inputs that reached the console outside emit (#417 r3): a 100,000-character `since` (it is in a group
#              title) and a `DIAG_DIR` that cannot be created (its path is in the error)
#   D6D        (same) the DIAG_DIR variant
#   D6B        the bound is not a call-site discipline: a copy of diag.sh with a direct 100 KB write injected after every
#              group marker still stays under the budget, and says so
#   D6P        a caller that CAPTURES the output through a pipe (subprocess.run(stdout=PIPE)) gets EOF and the exit status at once:
#              nothing of diag.sh outlives its exit holding that pipe (#417 r4)
#   D6F        the collector has no descriptor for the console (a scan of its descriptors finds none that is the console) and a
#              write to >&3 reaches nothing (#417 r4)
#   D6T        TERM while diag.sh collects: exit 143, what was printed before it is flushed, no fifo / writer left
#   D7  SQL text never leaves       : a real Postgres runs SQL with a synthetic credential literal while a lock is held;
#                                     nothing diag.sh prints or writes carries it (#420)
#
# ONLY=D5,D6 runs just those cells (a RED run of one cell must not be hidden behind another cell's failure).

# Every "output has X" is checked against text the fakes produce for THIS call (a nonce per run), so an empty or
# stale output cannot pass.
set -euo pipefail
. "$(dirname "$0")/../../../scripts/best-effort.sh"   # best_effort: diagnostics / cleanup / wait-gone only (#462); capture, value_of
abs_dir_of() { local p=${1%/*}; [ "$p" != "$1" ] || p=.; ( cd "$p${2:+/$2}" && pwd ); }   # the directory of $0 (plus a subdirectory), absolute: no `cd … && pwd` and no dirname inside $(…) (#470, #472)
here=$(abs_dir_of "$0") || exit 1
tmp=$(mktemp -d) || exit 1
. "$here/runner-home-test.sh" "$tmp/runner-home"  # the scripts refuse to start outside run.sh (#444)
WATCHDOG=""
trap 'test -z "$WATCHDOG" || kill $WATCHDOG 2>/dev/null; rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
nonce="n$$-$RANDOM"
since="2026-11-03T04:17:46Z"
printf 'secrets:\n  apiKeys: "shopper=kind-shopper-key-0123456789ab,admin=kind-admin-key-0123456789abc:admin"\n' > "$tmp/flow.yaml"
: > "$tmp/values.yaml"
only() { [ -z "${ONLY:-}" ] || [[ ",$ONLY," == *",$1,"* ]]; }

# fake kubectl: MODE=ok answers everything; MODE=degraded fails the log of pod/a, the locks read and nothing else;
# MODE=nopods fails the pod list. Every call is appended to $EVENTS (shared with the fake helm, so order is visible).
cat > "$tmp/bin/kubectl" <<'K'
#!/usr/bin/env bash
echo "kubectl $*" >> "$EVENTS"
[ -z "${SLOW:-}" ] || sleep "$SLOW"
args="$*"
case $args in
  *port-forward*) exit 0 ;;
  *"get pods -o name"*)
    [ "${MODE:-ok}" = nopods ] && { echo "error: the server is currently unable to handle the request" >&2; exit 1; }
    if [ -n "${PODCOUNT:-}" ]; then for i in $(seq 1 "$PODCOUNT"); do echo "pod/p$i"; done
    elif [ -n "${PGC:-}" ]; then printf 'pod/a\npod/postgres-0\n'
    else printf 'pod/a\npod/b\n'; fi ;;
  *"get pods -o wide"*)
    if [ -n "${WIDELINES:-}" ]; then seq 1 "$WIDELINES" | sed "s|^|PODS-$NONCE row |"; else echo "PODS-$NONCE"; fi ;;
  *"get events"*)
    if [ -n "${EVENTLINES:-}" ]; then
      for i in $(seq 1 "$EVENTLINES"); do printf 'EVENTS-%s %s ' "$NONCE" "$i"; head -c "${EVENTWIDTH:-100}" /dev/zero | tr '\0' 'e'; echo; done
    else echo "EVENTS-$NONCE"; fi ;;
  *"get pods"*) echo "PODS-$NONCE" ;;
  *" logs "*)
    pod=$(sed -n 's/.* logs \([^ ]*\) .*/\1/p' <<<"$args")
    st=$(sed -n 's/.*--since-time=\([^ ]*\).*/\1/p' <<<"$args")
    if [[ $args == *--previous* ]]; then
      [ "$pod" = pod/b ] && echo "2026-11-03T04:18:00Z earlier-instance $NONCE"
      exit 0
    fi
    if [ -n "${PGC:-}" ] && [ "$pod" = pod/postgres-0 ]; then
      "$RT" logs "$PGC" 2>&1 | sed "s/^/2026-11-03T04:18:02Z /"; exit 0
    fi
    if [ "${MODE:-ok}" = degraded ] && [ "$pod" = pod/a ]; then echo "unable to retrieve container logs" >&2; exit 1; fi
    if [ -n "${LONGLINE:-}" ]; then
      printf 'LONG-%s %s ' "$NONCE" "$pod"; head -c "$LONGLINE" /dev/zero | tr '\0' 'x'; echo
    elif [ -n "${LOGLINES:-}" ]; then
      seq 1 "$LOGLINES" | sed "s|^|2026-11-03T04:18:01Z LOG $pod line |; s|\$| $NONCE|"
    else
      echo "2026-11-03T04:18:01Z LOG $pod since=$st $NONCE"
    fi ;;
  *"exec deploy/postgres"*)
    if [ -n "${PGC:-}" ]; then
      rest=("$@"); while [ "${rest[0]}" != -- ]; do rest=("${rest[@]:1}"); done; rest=("${rest[@]:1}")
      exec "$RT" exec -i "$PGC" "${rest[@]}"
    fi
    last=${args##*-c }
    if [ -n "${SQLLINES:-}" ]; then seq 1 "$SQLLINES" | sed "s|^|SQL-$NONCE row |"; exit 0; fi
    if [ "${MODE:-ok}" = degraded ] && [[ $last == *pg_locks* ]]; then echo "psql: error: connection to server failed" >&2; exit 2; fi
    echo "SQL $NONCE: $last" ;;
  *) ;;
esac
K
cat > "$tmp/bin/helm" <<'H'
#!/usr/bin/env bash
echo "helm $*" >> "$EVENTS"
# HELM_BLOCK=<dir>: the FIRST upgrade waits (a helm --wait that does not return), leaving its pid and a marker
if [ -n "${HELM_BLOCK:-}" ] && [ ! -e "$HELM_BLOCK/started" ]; then
  echo $$ > "$HELM_BLOCK/helm.pid"; : > "$HELM_BLOCK/started"
  sleep 120 & wait $!
fi
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

die() { echo "diag-test: $*" >&2; echo "---- output:" >&2; best_effort cat "$tmp/out" >&2; echo "---- events:" >&2; best_effort cat "$tmp/events" >&2; exit 1; }
has() { grep -qF -- "$1" "$tmp/out" || die "$2: output lacks '$1'"; }
# ready_lines <runtime> <container>: ONE read of the Postgres log, the number of "ready" lines (grep -c: 1 = none, the count is 0).
# ready_twice: that count is at least 2. A failed read is a failure (nonzero, said on stderr), never a count of 0: the caller's loop retries, and a
# server that never gets there is a failure of the wait (the timeout), not a pass.
ready_lines() { "$1" logs "$2" 2>&1 | grep -c 'database system is ready to accept connections'; }
ready_twice() {
  local n
  n=$(value_of -ok 1 ready_lines "$1" "$2") || return
  [ "$n" -ge 2 ]
}
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
if only D1; then
export DIAG_DIR="$tmp/diag1"
MODE=ok run_bounded 60 "$here/diag.sh" ns "$since"
[ "$rc" = 0 ] || die "D1: exit $rc, want 0"
for pod in pod/a pod/b; do
  has "LOG $pod since=$since $nonce" "D1 the log of $pod since the given time"
done
has "pod/b: the previous container's log: 1 lines in $DIAG_DIR/log-b.previous.txt" "D1 the previous container's log is announced (its text is in the file)"
has "PODS-$nonce" "D1 pods"; has "EVENTS-$nonce" "D1 events"
for want in pg_stat_activity pg_blocking_pids pg_locks max_connections; do
  grep -qF "$want" "$tmp/events" || die "D1: no Postgres read of $want"
  has "SQL $nonce:" "D1 the Postgres reads answered"
done
n=$(value_of -ok 1 grep -c 'exec deploy/postgres' "$tmp/events") || exit 1; [ "$n" = 3 ] || die "D1: $n Postgres reads, want 3 (sessions with blocked_by, locks, connections)"
# the whole log is in files, not only on the console
for pod in a b; do grep -qF "LOG pod/$pod since=$since $nonce" "$DIAG_DIR/log-$pod.txt" || die "D1: $DIAG_DIR/log-$pod.txt lacks the log of pod/$pod"; done
grep -qF "earlier-instance $nonce" "$DIAG_DIR/log-b.previous.txt" || die "D1: no previous container file for pod/b"
grep -qF "SQL $nonce:" "$DIAG_DIR/postgres.txt" || die "D1: postgres.txt lacks this call's reads"
grep -q -- '--tail' "$tmp/events" && die "D1: a log was cut with --tail"
grep -q "statement_timeout = '10s'" "$tmp/events" || die "D1: the Postgres reads are not bounded on the server"
grep -q 'default_transaction_read_only = on' "$tmp/events" || die "D1: the Postgres reads are not read-only"
grep -q -- '--request-timeout=30s' "$tmp/events" || die "D1: kubectl calls are not bounded"
# the text of a query is SQL as the client sent it: no read selects it, in any spelling
grep -Eq '(SELECT|,)[[:space:]]*([a-z]+\.)?query([[:space:]]*,|[[:space:]]+FROM)|regexp_replace\(' "$tmp/events" && die "D1: a Postgres read selects the text of a query"
unset DIAG_DIR
echo "diag-test ok: D1 the whole log since the start as files, the previous container, three bounded read-only Postgres reads, no query text"
fi

# ── D2 diag.sh, parts fail ───────────────────────────────────────────────────
if only D2; then
export DIAG_DIR="$tmp/diag2"
MODE=degraded run_bounded 60 "$here/diag.sh" ns "$since"
[ "$rc" = 0 ] || die "D2: exit $rc, want 0 (diag must not fail the run it reports on)"
has "pod/a: diag: no log" "D2 a pod without a log is named"
has "LOG pod/b since=$since $nonce" "D2 the other pod's log is still there"
has "diag: 'locks (pg_locks)' failed" "D2 the failing read is named"
has "SQL $nonce:" "D2 the other reads still ran"
n=$(value_of -ok 1 grep -c 'exec deploy/postgres' "$tmp/events") || exit 1; [ "$n" = 3 ] || die "D2: $n Postgres reads, want 3 (a failing one must not stop the rest)"
MODE=nopods run_bounded 60 "$here/diag.sh" ns "$since"
[ "$rc" = 0 ] || die "D2: no pod list: exit $rc, want 0"
has "diag: no pod list" "D2 no pod list is said, not silently empty"
has "SQL $nonce:" "D2 no pod list: Postgres reads still ran"
unset DIAG_DIR
echo "diag-test ok: D2 a missing log, a failing read, no pod list: exit 0, the rest still printed"
fi

# ── D3 payment-flow.sh through the real entry ────────────────────────────────
if only D3; then
DIAG_DIR="$tmp/diag3" MODE=ok run_bounded 120 "$here/payment-flow.sh" ns rel chart "$tmp/values.yaml" "$tmp/flow.yaml"
[ "$rc" = 1 ] || die "D3: exit $rc, want 1 (the script's own status, not diag's or restore's)"
has "payment-flow: UpsertProduct: got '500', want '200'" "D3 the step that failed"
n=$(value_of -ok 1 grep -c '^::group::diag: pods and events' "$tmp/out") || exit 1; [ "$n" = 1 ] || die "D3: diag ran $n times, want exactly once"
start=$(sed -n 's/.*--since-time=\([^ ]*\).*/\1/p' "$tmp/events" | sort -u) || exit 1
[ -n "$start" ] || die "D3: no log was read at all (diag.sh did not run)"
start_lines=$(wc -l <<<"$start" | tr -d ' ') || exit 1
[ "$start_lines" = 1 ] || die "D3: the logs were read since more than one time: $start"
[[ $start =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || die "D3: since-time '$start' is not RFC 3339 UTC"
has "LOG pod/a since=$start $nonce" "D3 the pod log reached the output"
first_logs=$(awk '/ logs /{print NR; exit}' "$tmp/events") || exit 1
last_helm=$(awk '/^helm upgrade/{n=NR} END{if(n) print n}' "$tmp/events") || exit 1
first_helm=$(awk '/^helm upgrade/{print NR; exit}' "$tmp/events") || exit 1
[ -n "$first_logs" ] && [ -n "$last_helm" ] || die "D3: no log read or no restore in the events"
[ "$first_helm" -lt "$first_logs" ] || die "D3: the flow's own helm upgrade did not come first (the fake run did not get as far as it should)"
[ "$first_logs" -lt "$last_helm" ] || die "D3: the pods' logs were read AFTER the release was restored (helm upgrade replaced them)"
echo "diag-test ok: D3 a failing step: diag ran once, since $start, before the release was restored; exit 1"
fi

# ── D4 the kind Postgres writes the server log a lock wait needs ─────────────
if only D4; then
# PyYAML is already what deployments/helm/check.py needs, so this adds no tool
args=$(python3 - "$here/deps.yaml" <<'PY'
import sys, yaml
for doc in yaml.safe_load_all(open(sys.argv[1])):
    if doc and doc.get("kind") == "Deployment" and doc["metadata"]["name"] == "postgres":
        print(" ".join(doc["spec"]["template"]["spec"]["containers"][0].get("args", [])))
PY
) || exit 1
[ -n "$args" ] || die "D4: the postgres container of deps.yaml has no args"
for want in "log_lock_waits=on" "deadlock_timeout=1s" "log_checkpoints=on" "log_min_error_statement=panic" "log_min_messages=log"; do
  [[ " $args " == *" $want "* ]] || die "D4: postgres args lack $want (have: $args)"
done
# a statement log carries SQL text (literals included): the manifest must not turn one on
for never in log_min_duration_statement log_statement log_duration; do
  [[ " $args " != *" $never="* ]] || die "D4: postgres args set $never, which writes SQL text to the server log (have: $args)"
done
echo "diag-test ok: D4 postgres logs lock waits (1s) and checkpoints, keeps statements and SQL text out of the server log"
fi

# ── D5 TERM / INT while a helm upgrade waits ─────────────────────────────────
# python launches the script in a session of its own (a background job of a non-interactive shell ignores SIGINT, and
# an ignored signal cannot be trapped), signals ONLY the script's pid once the blocked helm says it started, and then
# reports the exit status and whether anything of that session is still alive.
cat > "$tmp/sigrun.py" <<'PY'
import os, signal, subprocess, sys, time
sig = getattr(signal, "SIG" + sys.argv[1]); marker = sys.argv[2]; cmd = sys.argv[4:]
# a test that was itself started in the background (SIGINT ignored, and inherited) must still deliver a catchable INT
p = subprocess.Popen(cmd, start_new_session=True, preexec_fn=lambda: signal.signal(signal.SIGINT, signal.SIG_DFL))
t0 = time.time()
while not os.path.exists(marker):
    if p.poll() is not None or time.time() - t0 > 30:
        print("rc=%s signalled=no" % p.poll()); sys.exit(0)
    time.sleep(0.1)
os.kill(p.pid, sig)
try:
    rc = p.wait(timeout=int(sys.argv[3]))
except subprocess.TimeoutExpired:
    os.killpg(p.pid, signal.SIGKILL); p.wait(); print("rc=timeout signalled=yes leftover=?"); sys.exit(0)
time.sleep(0.5)
left = 0
try:
    os.killpg(p.pid, 0); left = 1; os.killpg(p.pid, signal.SIGKILL)
except ProcessLookupError:
    pass
print("rc=%d signalled=yes leftover=%d" % (rc, left))
PY
for sg in TERM INT; do
  if only D5; then
    case $sg in TERM) want_rc=143 ;; INT) want_rc=130 ;; esac
    hb="$tmp/hb-$sg"; mkdir -p "$hb"; : > "$tmp/events"
    sig_rc=0
    res=$(DIAG_DIR="$tmp/diag5-$sg" EVENTS="$tmp/events" NONCE="$nonce" HELM_BLOCK="$hb" PATH="$tmp/bin:$PATH" python3 "$tmp/sigrun.py" $sg "$hb/started" 60 \
      "$here/payment-flow.sh" ns rel chart "$tmp/values.yaml" "$tmp/flow.yaml" 2>"$tmp/out") || sig_rc=$?
    [ "$sig_rc" = 0 ] || echo "diag-test: D5 $sg: sigrun.py exited $sig_rc (it reports the script's rc= and signalled= in its output, asserted next)" >&2
    echo "$res" > "$tmp/out"
    [[ $res == *"signalled=yes"* ]] || die "D5 $sg: the script never reached the waiting helm upgrade ($res)"
    [[ $res == *"rc=$want_rc "* ]] || die "D5 $sg: $res; want rc=$want_rc (128 + the signal)"
    [[ $res == *"leftover=0"* ]] || die "D5 $sg: a child of the script outlived it ($res)"
    hp=$(cat "$hb/helm.pid" 2>/dev/null) || { echo "diag-test: D5 $sg: cannot read the fake helm's pid file" >&2; exit 1; }
    [ -n "$hp" ] || die "D5 $sg: the fake helm left no pid"
    ! kill -0 "$hp" 2>/dev/null || { kill "$hp" 2>/dev/null; die "D5 $sg: the waiting helm (pid $hp) was left running"; }
    n=$(value_of -ok 1 grep -c ' logs ' "$tmp/events") || exit 1; [ "$n" -gt 0 ] || die "D5 $sg: diag.sh did not run (no pod log was read)"
    first_logs=$(grep -n ' logs ' "$tmp/events" | head -1 | cut -d: -f1) || exit 1
    last_helm=$(grep -n '^helm upgrade' "$tmp/events" | tail -1 | cut -d: -f1) || exit 1
    [ "$first_logs" -lt "$last_helm" ] || die "D5 $sg: diag.sh ran after the restore"
    _c275a=$(value_of -ok 1 grep -c '^helm upgrade' "$tmp/events") || exit 1; [ "$_c275a" -ge 2 ] || die "D5 $sg: the release was not restored (no second helm upgrade)"
    echo "diag-test ok: D5 $sg while helm upgrade waits: diag before restore, exit $want_rc, the waiting helm stopped, nothing left"
  fi
done

# ── D6 bounds ────────────────────────────────────────────────────────────────
if only D6; then
  # many pods x many lines: the console stays small, the files are complete
  export DIAG_DIR="$tmp/diag6"
  PODCOUNT=30 LOGLINES=5000 run_bounded 120 "$here/diag.sh" ns "$since"
  [ "$rc" = 0 ] || die "D6: exit $rc, want 0"
  lines=$(wc -l < "$tmp/out" | tr -d ' ') || exit 1
  [ "$lines" -le 4000 ] || die "D6: the console got $lines lines for 30 pods x 5000 lines (150000 in all), want at most 4000"
  [ -d "$DIAG_DIR" ] || die "D6: no artifact directory at $DIAG_DIR"
  full=0; for f in "$DIAG_DIR"/log-p*.txt; do [ -e "$f" ] || continue; f_lines=$(wc -l < "$f" | tr -d ' ') || exit 1; [ "$f_lines" = 5000 ] && full=$((full + 1)); done
  [ "$full" = 30 ] || die "D6: $full of 30 pod log files hold all 5000 lines"
  has "$DIAG_DIR" "D6 the console says where the files are"
  has "$nonce" "D6 the console shows pod log lines of this call"
  # a slow cluster: the collection stops at its deadline, says so, still exits 0
  rm -rf "$DIAG_DIR"
  began=$SECONDS
  SLOW=1 PODCOUNT=30 DIAG_DEADLINE=6 run_bounded 120 "$here/diag.sh" ns "$since"
  took=$((SECONDS - began))
  [ "$rc" = 0 ] || die "D6 deadline: exit $rc, want 0"
  [ "$took" -le 14 ] || die "D6 deadline: took ${took}s with a 6s deadline"
  has "diag: the 6s deadline was reached" "D6 deadline: the console says the collection stopped"
  got=$(ls "$DIAG_DIR"/log-p*.txt 2>/dev/null | wc -l | tr -d ' ') || exit 1
  [ "$got" -ge 1 ] && [ "$got" -lt 30 ] || die "D6 deadline: $got of 30 pod logs written; want some, not all (the deadline must cut)"
  unset DIAG_DIR
  echo "diag-test ok: D6 30 pods x 5000 lines: $lines console lines, 30 complete files; 6s deadline over a 1s-per-call cluster: ${took}s, $got pods"
fi

# ── D6L / D6W / D6E / D6S the console in bytes ───────────────────────────────
# One surface at a time (one fixture, one variable), each oversized past the budget in a way a line count does not see.
budget=65536   # DIAG_CONSOLE_BYTES for these cells: small, so the fixtures stay small too
slack=4096     # the fixed lines around the output (group markers, headers, the truncation notice)
d6cell() { # <cell> <label> <env assignments...>: run diag.sh with the fixture, check console bytes, truncation notice, files
  local cell=$1 label=$2; shift 2
  export DIAG_DIR="$tmp/diag-$cell"; rm -rf "$DIAG_DIR"
  local a; for a in "$@" DIAG_CONSOLE_BYTES=$budget; do export "$a"; done
  run_bounded 120 "$here/diag.sh" ns "$since"
  for a in "$@" DIAG_CONSOLE_BYTES=$budget; do unset "${a%%=*}"; done
  [ "$rc" = 0 ] || die "$cell: exit $rc, want 0"
  wc -c < "$tmp/out" > "$tmp/out.n" || die "$cell: cannot count the console bytes"
  read -r bytes < "$tmp/out.n" || die "$cell: cannot read the console byte count"
  [ "$bytes" -le $((budget + slack)) ] || die "$cell ($label): the console got $bytes bytes, want at most $((budget + slack)) (budget $budget)"
  has "console budget" "$cell: the console says it was cut"
  has "$DIAG_DIR" "$cell: the console says where the files are"
}
if only D6L; then
  # 30 pods, one 1 MiB line each: 76 console LINES, 31 MB
  d6cell D6L "30 pods x one 1 MiB line" PODCOUNT=30 LONGLINE=1048576
  for f in "$DIAG_DIR"/log-p*.txt; do f_bytes=$(wc -c < "$f" | tr -d ' ') || exit 1; [ "$f_bytes" -ge 1048576 ] || die "D6L: $f is cut (the file must hold the whole line)"; done
  n_files=$(ls "$DIAG_DIR"/log-p*.txt | wc -l | tr -d ' ') || exit 1
  [ "$n_files" = 30 ] || die "D6L: not all 30 pod files were written"
  unset DIAG_DIR
  echo "diag-test ok: D6L 30 pods x one 1 MiB line: $bytes console bytes (budget $budget), 30 complete files"
fi
if only D6M; then
  # every part fits its own cap (80 lines of ~80 bytes per pod, under 8 KiB) and no part is cut on its own: only the SUM is over
  # the budget. The branch of `emit` that counts what it let through is exercised by nothing else.
  d6cell D6M "30 pods, each under its own cap, together over the budget" PODCOUNT=30 LOGLINES=80
  has "is used up" "D6M: the total budget ran out, not a per-part cap"
  n_files=$(ls "$DIAG_DIR"/log-p*.txt | wc -l | tr -d ' ') || exit 1
  [ "$n_files" = 30 ] || die "D6M: not all 30 pod files were written after the console stopped"
  for f in "$DIAG_DIR"/log-p*.txt; do f_lines=$(wc -l < "$f" | tr -d ' ') || exit 1; [ "$f_lines" = 80 ] || die "D6M: $f does not hold all 80 lines"; done
  unset DIAG_DIR
  echo "diag-test ok: D6M 30 pods each under its own cap: $bytes console bytes (budget $budget), the console stopped, 30 complete files"
fi
if only D6W; then
  d6cell D6W "pods wide with 5000 rows" WIDELINES=5000
  pod_rows=$(wc -l < "$DIAG_DIR/pods.txt" | tr -d ' ') || exit 1
  [ "$pod_rows" = 5000 ] || die "D6W: pods.txt does not hold all 5000 rows"
  unset DIAG_DIR
  echo "diag-test ok: D6W pods wide, 5000 rows: $bytes console bytes (budget $budget), pods.txt complete"
fi
if only D6E; then
  d6cell D6E "events: 30 lines of 100 KiB" EVENTLINES=30 EVENTWIDTH=102400
  event_rows=$(wc -l < "$DIAG_DIR/events.txt" | tr -d ' ') || exit 1
  [ "$event_rows" = 30 ] || die "D6E: events.txt does not hold all 30 events"
  unset DIAG_DIR
  echo "diag-test ok: D6E 30 events of 100 KiB: $bytes console bytes (budget $budget), events.txt complete"
fi
if only D6S; then
  d6cell D6S "Postgres reads with 100000 rows" SQLLINES=100000
  _c355a=$(value_of -ok 1 grep -c "SQL-$nonce row" "$DIAG_DIR/postgres.txt") || exit 1; [ "$_c355a" -ge 100000 ] || die "D6S: postgres.txt does not hold the rows"
  unset DIAG_DIR
  echo "diag-test ok: D6S 100000 Postgres rows: $bytes console bytes (budget $budget), postgres.txt complete"
fi

# ── D6X / D6B / D6T the bound at the process boundary (#417 r3) ───────────────
if only D6X; then
  # the reviewer's first reproduction: 100,824 console bytes against a limit of 69,632 (the since is in a group title)
  sevens=$(head -c 100000 /dev/zero | tr '\0' 7) || exit 1
  longs="2026-11-03T00:00:00.${sevens}Z"
  export DIAG_DIR="$tmp/diag-X1" DIAG_CONSOLE_BYTES=$budget
  run_bounded 120 "$here/diag.sh" ns "$longs"
  unset DIAG_DIR DIAG_CONSOLE_BYTES
  [ "$rc" = 0 ] || die "D6X: exit $rc, want 0"
  bytes=$(wc -c < "$tmp/out" | tr -d ' ') || exit 1
  [ "$bytes" -le $((budget + slack)) ] || die "D6X: a 100,000-character since put $bytes bytes on the console, want at most $((budget + slack))"
  [ -s "$tmp/diag-X1/log-a.txt" ] || die "D6X: the collection itself did not run (no pod log in the files)"
  echo "diag-test ok: D6X a 100,000-character since: $bytes console bytes (limit $((budget + slack))), the files are there"
fi
if only D6D; then
  # the second: a DIAG_DIR that cannot be created has its whole path in the error (100,101 bytes)
  : > "$tmp/notadir"
  dees=$(head -c 100000 /dev/zero | tr '\0' d) || exit 1
  longd="$tmp/notadir/$dees"
  export DIAG_DIR="$longd" DIAG_CONSOLE_BYTES=$budget
  run_bounded 120 "$here/diag.sh" ns "$since"
  unset DIAG_DIR DIAG_CONSOLE_BYTES
  [ "$rc" = 0 ] || die "D6D: exit $rc, want 0"
  bytes=$(wc -c < "$tmp/out" | tr -d ' ') || exit 1
  [ "$bytes" -le $((budget + slack)) ] || die "D6D: a DIAG_DIR that cannot be created put $bytes bytes on the console, want at most $((budget + slack))"
  has "diag: cannot create" "D6D: the failure is still said"
  echo "diag-test ok: D6D a 100,000-character DIAG_DIR that cannot be created: $bytes console bytes (limit $((budget + slack)))"
fi
if only D6B; then
  # "the total budget holds whatever the script prints": inject a direct 100 KB write (not through emit, not through say) after
  # every group marker of a copy of diag.sh. If the bound were a per-call-site discipline this would blow through it.
  mut="$tmp/diag-mutant.sh"
  awk '{ print } /^(group|endgroup)/ { print "head -c 102400 /dev/zero | tr \"\\0\" X; echo hit >> \"$INJECT_HITS\"" }' "$here/diag.sh" > "$mut"
  chmod +x "$mut"
  _c392a=$(value_of -ok 1 grep -c 'INJECT_HITS' "$mut") || exit 1; [ "$_c392a" -ge 4 ] || die "D6B: the mutant was not built (no injections)"
  : > "$tmp/hits"; export INJECT_HITS="$tmp/hits" DIAG_DIR="$tmp/diag-B" DIAG_CONSOLE_BYTES=$budget
  run_bounded 120 "$mut" ns "$since"
  unset INJECT_HITS DIAG_DIR DIAG_CONSOLE_BYTES
  [ "$rc" = 0 ] || die "D6B: exit $rc, want 0"
  hits=$(wc -l < "$tmp/hits" | tr -d ' ') || exit 1
  [ "$hits" -ge 4 ] || die "D6B: only $hits injected writes ran (the mutant did not do what the cell assumes)"
  bytes=$(wc -c < "$tmp/out" | tr -d ' ') || exit 1
  [ "$bytes" -le $((budget + slack)) ] || die "D6B: $hits direct 100 KB writes put $bytes bytes on the console, want at most $((budget + slack))"
  has "console budget" "D6B: the console says the budget was used up"
  echo "diag-test ok: D6B $hits direct 100 KB writes injected into diag.sh: $bytes console bytes (limit $((budget + slack)))"
fi
if only D6P; then
  # the reviewer's reproduction: Popen(stdout=PIPE).communicate() against the unmodified script timed out at 5 s while
  # poll() already said the script had exited 0. The writer waited for the script's pid to vanish; the caller waited for EOF
  # before reaping it (a zombie still answers kill -0).
  cat > "$tmp/pipecap.py" <<'PY'
import subprocess, sys, time
t0 = time.time()
p = subprocess.Popen(sys.argv[1:], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
try:
    out, _ = p.communicate(timeout=20)
    print("rc=%d bytes=%d secs=%.1f" % (p.returncode, len(out), time.time() - t0))
except subprocess.TimeoutExpired:
    p.kill(); p.communicate()
    print("rc=%s bytes=? secs=timeout" % p.poll())
PY
  res=$(DIAG_DIR="$tmp/diag-P" EVENTS="$tmp/events" NONCE="$nonce" PATH="$tmp/bin:$PATH" DIAG_CONSOLE_BYTES=$budget \
    python3 "$tmp/pipecap.py" bash "$here/diag.sh" ns "$since") || exit 1
  [[ $res == *"secs=timeout"* ]] && die "D6P: a pipe-capturing caller never got EOF ($res): the writer waits for its caller, the caller for the writer"
  [[ $res == "rc=0 "* ]] || die "D6P: $res; want rc=0"
  nb=$(sed -n 's/.*bytes=\([0-9]*\).*/\1/p' <<<"$res") || exit 1
  [ "${nb:-0}" -gt 0 ] || die "D6P: the caller captured nothing ($res): the output did not reach the pipe"
  [ "$nb" -le $((budget + slack)) ] || die "D6P: the caller captured $nb bytes, want at most $((budget + slack))"
  echo "diag-test ok: D6P a caller capturing through a pipe: $res"
fi
if only D6F; then
  # the collector must have no way to the console. Inject, right below the marker comment that opens the collector, (a) a write
  # to descriptor 3 (what an earlier version inherited for the real console) and (b) a scan of every descriptor the collector
  # has for one that is the very file the console is (the test's own capture of diag.sh's stdout). The scan compares
  # (st_dev, st_ino) from python: `[ /dev/fd/N -ef file ]` finds nothing on macOS (its /dev/fd/N is a new node), so a shell scan
  # would pass whatever the collector held.
  cat > "$tmp/fdscan.py" <<'PY'
import os, sys
c = os.stat(sys.argv[1])
for fd in range(0, 256):
    try:
        s = os.fstat(fd)
    except OSError:
        continue
    if (s.st_dev, s.st_ino) == (c.st_dev, c.st_ino):
        print(fd)
PY
  # positive control: a process whose stdout IS the console must be seen by the scan, or the scan proves nothing
  : > "$tmp/ctl-console"
  python3 "$tmp/fdscan.py" "$tmp/ctl-console" > "$tmp/ctl-console"
  ctl_seen=$(cat "$tmp/ctl-console") || exit 1
  [ "$ctl_seen" = 1 ] || die "D6F: the descriptor scan did not see its own stdout as the console (got: $ctl_seen); it would pass whatever the collector held"
  mut="$tmp/diag-fd3.sh"
  awk '{ print } /^# diag-test D6F injects/ { print "printf \"%100000s\" X >&3 2>/dev/null; echo hit >> \"$INJECT_HITS\""; print "echo scanned >> \"$INJECT_SCANNED\"; python3 \"$INJECT_SCAN\" \"$INJECT_CONSOLE\" >> \"$INJECT_FDS\""}' "$here/diag.sh" > "$mut"
  chmod +x "$mut"
  _c452a=$(value_of -ok 1 grep -c 'INJECT_HITS' "$mut") || exit 1; [ "$_c452a" = 1 ] || die "D6F: the mutant was not built (the marker comment was not found)"
  : > "$tmp/hits-f"; : > "$tmp/scanned-f"; : > "$tmp/fds-f"
  export INJECT_HITS="$tmp/hits-f" INJECT_SCANNED="$tmp/scanned-f" INJECT_FDS="$tmp/fds-f" INJECT_SCAN="$tmp/fdscan.py" INJECT_CONSOLE="$tmp/out" DIAG_DIR="$tmp/diag-F" DIAG_CONSOLE_BYTES=$budget
  run_bounded 120 "$mut" ns "$since"
  unset INJECT_HITS INJECT_SCANNED INJECT_FDS INJECT_SCAN INJECT_CONSOLE DIAG_DIR DIAG_CONSOLE_BYTES
  [ "$rc" = 0 ] || die "D6F: exit $rc, want 0"
  hits=$(wc -l < "$tmp/hits-f" | tr -d ' ') || exit 1
  [ "$hits" = 1 ] || die "D6F: the injected write did not run"
  scans=$(wc -l < "$tmp/scanned-f" | tr -d ' ') || exit 1
  [ "$scans" = 1 ] || die "D6F: the descriptor scan did not run (it proves nothing if it did not)"
  if [ -s "$tmp/fds-f" ]; then
    capture fds_txt fds_rc tr '\n' ' ' < "$tmp/fds-f"   # the text of a failure message; the verdict (die) does not depend on it
    die "D6F: the collector holds a descriptor for the console: fd $fds_txt"
  fi
  bytes=$(wc -c < "$tmp/out" | tr -d ' ') || exit 1
  [ "$bytes" -le $((budget + slack)) ] || die "D6F: a write to the inherited console descriptor put $bytes bytes on the console, want at most $((budget + slack))"
  has "diag: files in" "D6F: the run itself still printed (the wrapper prints the capture)"
  echo "diag-test ok: D6F the collector has no console descriptor (scanned by inode, scan proven on a positive control) and a write to >&3 reaches nothing: $bytes console bytes (limit $((budget + slack)))"
fi
if only D6T; then
  # TERM while diag.sh is collecting (a slow cluster keeps it busy): 143, what it had printed is on the console, nothing is left
  cat > "$tmp/termrun.py" <<'PY'
import os, signal, subprocess, sys, time
marker, out = sys.argv[1], sys.argv[2]; cmd = sys.argv[3:]
f = open(out, "w")
p = subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT, start_new_session=True, preexec_fn=lambda: signal.signal(signal.SIGINT, signal.SIG_DFL))
t0 = time.time()
while not os.path.exists(marker) and time.time() - t0 < 30 and p.poll() is None: time.sleep(0.05)
os.kill(p.pid, signal.SIGTERM)
try: rc = p.wait(timeout=20)
except subprocess.TimeoutExpired: os.killpg(p.pid, signal.SIGKILL); p.wait(); print("rc=timeout leftover=?"); sys.exit(0)
time.sleep(0.5); left = 0
try: os.killpg(p.pid, 0); left = 1; os.killpg(p.pid, signal.SIGKILL)
except ProcessLookupError: pass
print("rc=%d leftover=%d" % (rc, left))
PY
  : > "$tmp/events"; rm -f "$tmp/started"
  # SLOW=1: every kubectl call takes 1 s; the first `logs` call proves diag.sh has printed its first lines
  mkdir -p "$tmp/tmpT"   # diag.sh's own TMPDIR: only ITS leftovers are counted below, not other runs' in the shared one
  res=$(TMPDIR="$tmp/tmpT" DIAG_DIR="$tmp/diag-T" EVENTS="$tmp/events" NONCE="$nonce" SLOW=1 PODCOUNT=30 PATH="$tmp/bin:$PATH" \
    python3 "$tmp/termrun.py" "$tmp/diag-T/pods.txt" "$tmp/out" "$here/diag.sh" ns "$since") || exit 1
  [[ $res == *"rc=143 "* ]] || die "D6T: $res; want rc=143 (128 + TERM)"
  [[ $res == *"leftover=0"* ]] || die "D6T: something of diag.sh outlived it ($res)"
  has "diag: files in" "D6T: what diag.sh printed before the TERM reached the console"
  left_fifos=$(find "$tmp/tmpT" -maxdepth 1 -name 'diag-console.*' | wc -l | tr -d ' ') || exit 1
  [ "$left_fifos" = 0 ] || die "D6T: $left_fifos console fifo dirs left in $tmp/tmpT"
  echo "diag-test ok: D6T TERM while collecting: $res, the printed part is on the console, no fifo left"
fi

# ── D7 SQL text never leaves ─────────────────────────────────────────────────
# Two places carry SQL text: pg_stat_activity.query (diag.sh reads it) and the server log (diag.sh passes the pod's log
# through UNFILTERED: a filter over a text channel misses the next log format, #415). So the first is kept out by what
# diag.sh selects, the second by the server's own flags in deps.yaml — and both are checked on a real Postgres running SQL
# with a synthetic credential as a literal, once with leaky flags as the positive control.
if only D7; then
  rt=$(value_of -ok 1 command -v docker) || exit 1   # command -v: 1 = not found (empty), anything else is a failure
  if [ -z "$rt" ]; then rt=$(value_of -ok 1 command -v podman) || exit 1; fi
  [ -n "$rt" ] || echo "diag-test: D7: neither docker nor podman is installed" >&2
  [ -n "$rt" ] || die "D7: needs docker or podman (a real Postgres is the point of this cell)"
  secret="sk_live_SYNTHETIC_$nonce"
  pgc=""; cleanup_pg() { [ -z "$pgc" ] || "$rt" rm -f "$pgc" >/dev/null 2>&1 || true; pgc=""; }
  trap 'cleanup_pg; test -z "$WATCHDOG" || kill $WATCHDOG 2>/dev/null; rm -rf "$tmp"' EXIT
  # d7 <label> <postgres flags...>: session H holds a lock on t and runs SQL with the secret as a literal, session W queues
  # behind it (its own SQL carries the secret), session U violates a unique index with the secret as the key value (an ERROR
  # whose DETAIL names the value); then diag.sh runs for real against that server.
  d7() {
    local label=$1; shift
    pgc="diagpg-$$-$RANDOM"
    HOME="$ENGINE_HOME" "$rt" run -d --name "$pgc" -e POSTGRES_USER=app -e POSTGRES_PASSWORD=app -e POSTGRES_DB=app postgres:16-alpine "$@" >/dev/null
    # The image starts a throwaway server to run its init, stops it, and starts the real one: pg_isready answers during the
    # first, and the connection is then cut ("terminating connection due to administrator command", seen on CI #417 r1 where
    # the image had to be pulled). Ready means the line appears TWICE; a server that never gets there is a setup failure.
    ready=0
    for ((_try518 = 1; _try518 <= 120; _try518++)); do
      if ready_twice "$rt" "$pgc"; then ready=1; break; fi
      sleep 1
    done
    [ "$ready" = 1 ] || die "D7 $label: the Postgres container never became ready (setup failure, not a result)"
    local psql=("$rt" exec -i "$pgc" psql -U app -d app -X -q)
    "${psql[@]}" -v ON_ERROR_STOP=1 -c "create table t(a text)" -c "create unique index on t(a)" -c "insert into t values ('seed')"
    dup_rc=0; "${psql[@]}" -c "insert into t values ('$secret-u')" -c "insert into t values ('$secret-u')" >/dev/null 2>&1 || dup_rc=$?
    [ "$dup_rc" != 0 ] || die "D7 $label: the duplicate insert was meant to violate the unique index and did not"
    "${psql[@]}" -c "begin; lock table t in access exclusive mode; select '$secret' as s, pg_sleep(40); commit" >/dev/null 2>&1 &
    local h=$!
    sleep 1
    "${psql[@]}" -c "insert into t values ('$secret-w')" >/dev/null 2>&1 &
    local w=$!
    sleep 3   # past the 1s lock-wait log of session W
    export DIAG_DIR="$tmp/diag7-$label"
    PGC="$pgc" RT="$rt" run_bounded 120 "$here/diag.sh" ns "$since"
    [ "$rc" = 0 ] || die "D7 $label: exit $rc, want 0"
    capture D7_RAW_QUERY d7_query_rc "${psql[@]}" -At -c "select string_agg(query, ' | ') from pg_stat_activity where query like '%$secret%' and pid <> pg_backend_pid()" || exit 1
    [ "$d7_query_rc" = 0 ] || { echo "diag-test: D7 $label: cannot read pg_stat_activity" >&2; exit 1; }
    best_effort "$rt" logs "$pgc" > "$tmp/d7-raw.log" 2>&1   # an unreadable container log is an empty one (and said so); the leak check below still reads diag.sh's own output
    D7_RAW_LOG=$(cat "$tmp/d7-raw.log") || { echo "diag-test: D7 $label: cannot read the saved container log" >&2; exit 1; }
    kill "$h" "$w" 2>/dev/null || true; wait "$h" "$w" 2>/dev/null || true
    D7_SEEN=$(cat "$tmp/out" "$DIAG_DIR"/* 2>/dev/null) || { echo "diag-test: D7: cannot read what diag.sh wrote — a leak check over nothing proves nothing" >&2; exit 1; }
    unset DIAG_DIR
  }
  # control: leaky flags. The secret is really there to be found: in pg_stat_activity, in the statement log, and in the
  # DETAIL of the unique violation.
  d7 control -c log_min_duration_statement=0 -c log_min_error_statement=error -c log_lock_waits=on -c deadlock_timeout=1s
  [[ $D7_RAW_QUERY == *"$secret"* ]] || die "D7 control: the secret is not in pg_stat_activity.query (the cell would prove nothing)"
  [[ $D7_RAW_LOG == *"$secret-w"* ]] || die "D7 control: the secret of a statement is not in the leaky server's log (the cell would prove nothing)"
  [[ $D7_RAW_LOG == *"Key (a)=($secret-u)"* ]] || die "D7 control: the unique violation's DETAIL does not carry the secret (the cell would prove nothing)"
  cleanup_pg
  # the manifest's own flags
  real=$(python3 - "$here/deps.yaml" <<'PY'
import sys, yaml
for doc in yaml.safe_load_all(open(sys.argv[1])):
    if doc and doc.get("kind") == "Deployment" and doc["metadata"]["name"] == "postgres":
        print("\n".join(doc["spec"]["template"]["spec"]["containers"][0].get("args", [])))
PY
) || exit 1
  [ -n "$real" ] || die "D7: no postgres args in deps.yaml"
  real_args=(); while IFS= read -r a; do real_args+=("$a"); done <<<"$real"
  d7 manifest "${real_args[@]}"
  [[ $D7_RAW_QUERY == *"$secret"* ]] || die "D7 manifest: the secret is not in pg_stat_activity.query (the cell would prove nothing)"
  [[ $D7_RAW_LOG != *"$secret"* ]] || die "D7 manifest: the server log written under deps.yaml's flags carries the synthetic credential"
  [[ $D7_RAW_LOG == *"still waiting for"* && $D7_RAW_LOG == *"Process holding the lock"* ]] || die "D7 manifest: the server log does not say who holds the lock (the flags no longer help)"
  [[ $D7_SEEN != *"$secret"* ]] || die "D7 manifest: diag.sh output or files carry the synthetic credential"
  [[ $D7_SEEN == *"Process holding the lock"* ]] || die "D7 manifest: the holder line did not reach diag's output (the pod log was not collected)"
  [[ $D7_SEEN == *blocked_by* ]] || die "D7 manifest: the sessions read has no blocked_by column"
  cleanup_pg
  echo "diag-test ok: D7 a synthetic credential in running SQL: raw in pg_stat_activity and in a leaky server's log; absent from everything diag.sh prints or writes, and from the log under deps.yaml's own flags (which still name the lock holder)"
fi

echo "diag-test ok"
