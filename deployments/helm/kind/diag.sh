#!/usr/bin/env bash
# What a failed kind run leaves behind for finding the cause (#416, #420):
#   diag.sh <namespace> <since: RFC 3339 UTC time the run started>
#
# FILES (DIAG_DIR, default $RUNNER_TEMP/kind-diag, else $TMPDIR or /tmp; CI uploads it as an artifact):
#   pods.txt events.txt            pods and events
#   log-<pod>.txt                  the WHOLE log of every pod since <since> (timestamps; at most DIAG_POD_BYTES, default 5 MB,
#   log-<pod>.previous.txt         per pod), and the previous container's where there is one
#   postgres.txt                   sessions, locks, connections (below)
# THE CONSOLE IS NEVER IN THE COLLECTOR'S HANDS (#417 r3, r4): this file is two programs. The WRAPPER runs first, is the only
#   holder of the real stdout / stderr, and starts the COLLECTOR (this same file, with --diag-child) with its stdout and stderr
#   already pointed at a capture FILE and its stdin at /dev/null. So nothing the collector or anything it starts can write
#   reaches the console: there is no descriptor for it to find (no fd 3, no fifo, no resident writer, no pid to wait for). When
#   the collector is done, or the wrapper gets TERM / INT / HUP, the wrapper prints the first DIAG_CONSOLE_BYTES (default 256
#   KiB) of the capture, says ONCE if there was more (naming the directory), copies the whole capture to $DIAG_DIR/console.full
#   and exits with the collector's status (128 + the signal for a signal). Cost: the console is not a live stream; it appears
#   when the run ends. emit and the per-part caps below only decide WHAT is worth the bytes in the capture.
# CONSOLE (the Actions log): where the files are, per pod the last DIAG_CONSOLE_LINES lines (default 100) cut to
#   DIAG_POD_CONSOLE_BYTES (8 KiB) and how many lines there are in all, pods / events / postgres cut the same way. A limit in
#   lines is no limit (one 1 MiB line is one line), so EVERYTHING written to the console goes through `emit`, which keeps one
#   byte budget for the whole run (DIAG_CONSOLE_BYTES, default 256 KiB): when it is used up it says so once and prints nothing
#   more, while the files are still written in full (#417 r2).
# POSTGRES reads are structured columns only: pid, application_name, state, wait event, seconds in the transaction / query,
#   the pids blocking it, and per lock its mode, whether it is granted and the relation's NAME. The text of a query is never
#   selected: it is SQL as the client sent it, literals included (a credential inserted by hand would be printed).
#   The server's own log (it comes with the pod's log) is kept free of SQL text by the flags in deps.yaml, not by a filter.
# BOUNDS: the whole collection stops at DIAG_DEADLINE seconds (default 120), each kubectl call is capped by what is left of it,
#   and it says so. It is called BEFORE the run restores the release: `helm upgrade` replaces the pods and their logs.
# Best effort: it never fails, so it cannot hide the failure it reports on. Needs kubectl.
set -uo pipefail
. "$(dirname "$0")/../../../scripts/best-effort.sh"   # best_effort: diagnostics / cleanup / wait-gone only (#462)
# refuses to run outside run.sh (#444): HOME is the runner's scratch directory, which holds the marker
[ "${KIND_RUNNER_HOME:-}" = "${HOME:-}" ] && [ -f "${HOME:-/nonexistent}/.kind-runner" ] || { echo "$0: run it through its task (task helm-kind-up / helm-kind / helm-kind-keda; CLUSTER=<name> picks the cluster): the task isolates it from ~/.kube/config (#444)" >&2; exit 2; }
dir=${DIAG_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/kind-diag}
console_budget=${DIAG_CONSOLE_BYTES:-262144} pod_console=${DIAG_POD_CONSOLE_BYTES:-8192}

# ── the wrapper ──────────────────────────────────────────────────────────────
if [ "${1:-}" != "--diag-child" ]; then
  cap_dir=$(mktemp -d "${TMPDIR:-/tmp}/diag-console.XXXXXX") || { echo "diag: cannot create a capture directory; nothing collected"; exit 0; }
  child="" sig_rc="" kill_pid="" c=""
  # on a signal: stop everything under the collector, then ask the collector itself to stop (it cleans up); if it is still there after 5 s, kill it
  children_of() {   # the direct children of pid $1, one per line. pgrep: 1 = no children (fine); 2+ = it failed: say so, carry on with none (a diagnostic stop must not stop on this)
    value_or_empty -ok 1 pgrep -P "$1"
  }
  stop_tree() {   # the whole subtree under $1, innermost first, by explicit pid (never by name)
    local x kids
    kids=$(children_of "$1") || return
    for x in $kids; do stop_tree "$x"; kill "$x" 2>/dev/null || true; done
  }
  on_signal() {
    sig_rc=$1
    trap '' TERM INT HUP
    stop_tree "$child"
    kill -TERM "$child" 2>/dev/null || true
    ( sleep 5; kill -KILL "$child" 2>/dev/null ) > /dev/null 2>&1 &
    kill_pid=$!
  }
  trap 'on_signal 143' TERM
  trap 'on_signal 130' INT
  trap 'on_signal 129' HUP
  bash "$0" --diag-child "$@" > "$cap_dir/console" 2>&1 < /dev/null &
  child=$!
  rc=0
  while :; do
    wait "$child"; rc=$?
    kill -0 "$child" 2>/dev/null || break   # reaped: it is gone. Still there: wait was interrupted by a trapped signal
  done
  if [ -n "$kill_pid" ]; then   # the 5 s fuse: its `sleep` first, then the subshell holding it (explicit pids). KILL, not TERM:
    # the fuse was started after on_signal ignored TERM, and an ignored signal is inherited (diag-test D6T saw it survive)
    kids=$(pgrep -P "$kill_pid" 2>/dev/null) || { [ $? -eq 1 ] || echo "diag: pgrep -P $kill_pid failed" >&2; }   # 1 = no children
    for c in $kids; do kill -KILL "$c" 2>/dev/null || true; done
    kill -KILL "$kill_pid" 2>/dev/null || true
  fi
  [ -z "$sig_rc" ] || rc=$sig_rc
  trap '' TERM INT HUP
  cap="$cap_dir/console"
  size=$(wc -c < "$cap" | tr -d ' ')
  head -c "$console_budget" "$cap"
  if [ "$size" -gt "$console_budget" ]; then
    printf '\n[diag: the console budget of %s bytes is used up; nothing more is printed here, the whole output is in %.200s/console.full]\n::endgroup::\n' "$console_budget" "$dir"
  fi
  [ ! -d "$dir" ] || best_effort cp -f -- "$cap" "$dir/console.full"
  case "$cap_dir" in */diag-console.*) rm -rf -- "$cap_dir" ;; esac
  exit "$rc"
fi
shift   # --diag-child

# ── the collector: its stdout and stderr are the capture file, nothing else ────
# diag-test D6F injects a write to a console descriptor right below this line: there is none to write to.
scratch=$(mktemp) || scratch=/dev/null
finish() { [ "$scratch" = /dev/null ] || rm -f -- "$scratch"; }
trap finish EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
trap 'exit 129' HUP

ns=${1:?usage: diag.sh <namespace> <since RFC3339>} since=${2:?usage: diag.sh <namespace> <since RFC3339>}
deadline=${DIAG_DEADLINE:-120} tail_n=${DIAG_CONSOLE_LINES:-100} max_bytes=${DIAG_POD_BYTES:-5000000}
mkdir -p "$dir" 2>/dev/null || { echo "diag: cannot create $dir; nothing collected"; exit 0; }

# emit's own accounting leaves room under the wrapper's cut for the fixed lines (group markers, notices), so on the ordinary
# path the wrapper never has to cut anything
reserve=4096; [ "$console_budget" -gt $((reserve * 4)) ] || reserve=$((console_budget / 4))
console_left=$((console_budget - reserve)) console_cut=""
# emit [max_bytes [file]]: stdin -> the console, under the run's byte budget and, if given, max_bytes for this call (the notice
# names the file that has all of it). The ONE way text reaches the console, except the constant ::group:: markers (raw): a
# cut never leaves a group open. Reads at most cap+1 bytes, so an enormous input costs nothing. It keeps the budget in shell
# variables, so it must run in THIS shell: feed it with `< file`, never `cmd | emit` (a pipeline runs it in a subshell and
# the budget would not add up; diag-test D6L is what noticed).
emit() {
  local cap=$console_left own=0 where=${2:-$dir}
  if [ -n "${1:-}" ] && [ "$1" -lt "$cap" ]; then cap=$1 own=1; fi
  if [ -n "$console_cut" ] || [ "$cap" -le 0 ]; then cat > /dev/null; return 0; fi
  local buf n; buf=$(mktemp) || { echo "diag: no temp file for this read, skipping it" >&2; return 0; }
  head -c $((cap + 1)) > "$buf"
  if ! wc -c < "$buf" > "$buf.n"; then echo "diag: cannot count this read, skipping it" >&2; rm -f "$buf" "$buf.n"; return 0; fi
  read -r n < "$buf.n" || n=0; rm -f "$buf.n"
  if [ "$n" -le "$cap" ]; then
    cat "$buf"; console_left=$((console_left - n))
  else
    head -c "$cap" "$buf"; echo
    console_left=$((console_left - cap))
    if [ "$own" = 1 ]; then
      printf '[diag: cut to the console budget of %s bytes for this part; all of it is in %.200s]\n' "$cap" "$where"
    else
      console_cut=1
      printf '[diag: the console budget of %s bytes is used up; nothing more is printed here, everything is in %.200s]\n' "$console_budget" "$dir"
    fi
  fi
  rm -f "$buf"
}
say() { printf '%s\n' "$*" > "$scratch"; emit < "$scratch"; }
# tail_emit <file> <lines> <bytes>: the last <lines> lines of <file>, cut to <bytes>, through emit. One byte more than the cap is
# handed over, so emit sees the overflow and says it cut (handing it exactly <bytes> would hide the cut).
tail_emit() { tail -n "$2" "$1" | tail -c $(($3 + 1)) > "$scratch"; emit "$3" "$1" < "$scratch"; }
group() { printf '::group::%s\n' "$1"; }
endgroup() { echo "::endgroup::"; }

left() { echo $((deadline - SECONDS)); }
# k <kubectl args>: bounded by 30 s and by what is left of the deadline (124 = out of time)
k() {
  local r; r=$(left)
  [ "$r" -gt 0 ] || return 124
  [ "$r" -le 30 ] || r=30
  kubectl --request-timeout="${r}s" -n "$ns" "$@"
}
cut_short=""
out_of_time() { [ "$(left)" -gt 0 ] && return 1; [ -n "$cut_short" ] || say "diag: the ${deadline}s deadline was reached; the rest is not collected"; cut_short=1; return 0; }

say "diag: files in $dir; the console shows the last $tail_n lines (at most $pod_console bytes) per pod, $console_budget bytes in all; deadline ${deadline}s"

group "diag: pods and events"
k get pods -o wide > "$dir/pods.txt" 2>&1 || echo "diag: kubectl get pods failed" >> "$dir/pods.txt"
emit 16384 "$dir/pods.txt" < "$dir/pods.txt"
k get events --sort-by=.lastTimestamp > "$dir/events.txt" 2>&1 || echo "diag: kubectl get events failed" >> "$dir/events.txt"
tail_emit "$dir/events.txt" 30 8192
endgroup

group "diag: pod logs since $since"
pods=$(k get pods -o name 2>/dev/null) || pods=""
[ -n "$pods" ] || say "diag: no pod list (kubectl get pods failed or the namespace is empty)"
for p in $pods; do
  out_of_time && break
  name=${p#pod/}
  f="$dir/log-$name.txt"
  if k logs "$p" --all-containers --timestamps --since-time="$since" --limit-bytes="$max_bytes" > "$f" 2>/dev/null; then
    say "--- $p: $(wc -l < "$f" | tr -d ' ') lines, $(wc -c < "$f" | tr -d ' ') bytes, all of them in $f"
    tail_emit "$f" "$tail_n" "$pod_console"
  else
    say "--- $p: diag: no log"
    rm -f "$f"
  fi
  prev="$dir/log-$name.previous.txt"
  if k logs "$p" --all-containers --timestamps --since-time="$since" --limit-bytes="$max_bytes" --previous > "$prev" 2>/dev/null && [ -s "$prev" ]; then
    say "--- $p: the previous container's log: $(wc -l < "$prev" | tr -d ' ') lines in $prev"
  else
    rm -f "$prev"
  fi
done
endgroup

group "diag: postgres sessions and locks"
pg="$dir/postgres.txt"; : > "$pg"
sql() { # <title> <statement>: read-only, bounded to 10 s on the server
  out_of_time && return 0
  echo "--- $1" >> "$pg"
  k exec deploy/postgres -- psql -U app -d app -X -q -P pager=off -v ON_ERROR_STOP=1 \
    -c "SET statement_timeout = '10s'" -c "SET default_transaction_read_only = on" -c "$2" >> "$pg" 2>&1 || echo "diag: '$1' failed" >> "$pg"
}
sql "sessions (pg_stat_activity, no query text)" "SELECT pid, application_name, state, wait_event_type, wait_event, EXTRACT(EPOCH FROM now() - xact_start)::int AS in_xact_s, EXTRACT(EPOCH FROM now() - query_start)::int AS in_query_s, pg_blocking_pids(pid) AS blocked_by FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() ORDER BY xact_start NULLS LAST, pid"
sql "locks (pg_locks)" "SELECT l.pid, l.locktype, l.mode, l.granted, c.relname FROM pg_locks l LEFT JOIN pg_class c ON c.oid = l.relation WHERE l.pid <> pg_backend_pid() ORDER BY l.granted, l.pid, c.relname"
sql "connections against max_connections" "SELECT count(*) AS connections, current_setting('max_connections') AS max_connections FROM pg_stat_activity"
emit 32768 "$pg" < "$pg"
endgroup
say "diag: done in ${SECONDS}s; $(ls "$dir" | wc -l | tr -d ' ') files in $dir"
exit 0
