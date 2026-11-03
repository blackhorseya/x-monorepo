#!/usr/bin/env bash
# What a failed kind run leaves behind for finding the cause (#416, #420):
#   diag.sh <namespace> <since: RFC 3339 UTC time the run started>
#
# FILES (DIAG_DIR, default $RUNNER_TEMP/kind-diag, else $TMPDIR or /tmp; CI uploads it as an artifact):
#   pods.txt events.txt            pods and events
#   log-<pod>.txt                  the WHOLE log of every pod since <since> (timestamps; at most DIAG_POD_BYTES, default 5 MB,
#   log-<pod>.previous.txt         per pod), and the previous container's where there is one
#   postgres.txt                   sessions, locks, connections (below)
# CONSOLE (the Actions log): where the files are, per pod the last DIAG_CONSOLE_LINES lines (default 100) and how many there
#   are in all, and postgres.txt. Nothing else: a 5,000-line pod must not bury the failure it belongs to.
# POSTGRES reads are structured columns only: pid, application_name, state, wait event, seconds in the transaction / query,
#   the pids blocking it, and per lock its mode, whether it is granted and the relation's NAME. The text of a query is never
#   selected: it is SQL as the client sent it, literals included (a credential inserted by hand would be printed).
#   The server's own log (it comes with the pod's log) is kept free of SQL text by the flags in deps.yaml, not by a filter.
# BOUNDS: the whole collection stops at DIAG_DEADLINE seconds (default 120), each kubectl call is capped by what is left of it,
#   and it says so. It is called BEFORE the run restores the release: `helm upgrade` replaces the pods and their logs.
# Best effort: it never fails, so it cannot hide the failure it reports on. Needs kubectl.
set -uo pipefail
ns=${1:?usage: diag.sh <namespace> <since RFC3339>} since=${2:?usage: diag.sh <namespace> <since RFC3339>}
dir=${DIAG_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/kind-diag}
deadline=${DIAG_DEADLINE:-120} tail_n=${DIAG_CONSOLE_LINES:-100} max_bytes=${DIAG_POD_BYTES:-5000000}
mkdir -p "$dir" 2>/dev/null || { echo "diag: cannot create $dir; nothing collected"; exit 0; }

left() { echo $((deadline - SECONDS)); }
# k <kubectl args>: bounded by 30 s and by what is left of the deadline (124 = out of time)
k() {
  local r; r=$(left)
  [ "$r" -gt 0 ] || return 124
  [ "$r" -le 30 ] || r=30
  kubectl --request-timeout="${r}s" -n "$ns" "$@"
}
cut_short=""
out_of_time() { [ "$(left)" -gt 0 ] && return 1; [ -n "$cut_short" ] || echo "diag: the ${deadline}s deadline was reached; the rest is not collected"; cut_short=1; return 0; }

echo "diag: files in $dir; the console shows the last $tail_n lines per pod; deadline ${deadline}s"

echo "::group::diag: pods and events"
k get pods -o wide > "$dir/pods.txt" 2>&1 || echo "diag: kubectl get pods failed" >> "$dir/pods.txt"
cat "$dir/pods.txt"
k get events --sort-by=.lastTimestamp > "$dir/events.txt" 2>&1 || echo "diag: kubectl get events failed" >> "$dir/events.txt"
tail -n 30 "$dir/events.txt"
echo "::endgroup::"

echo "::group::diag: pod logs since $since"
pods=$(k get pods -o name 2>/dev/null) || pods=""
[ -n "$pods" ] || echo "diag: no pod list (kubectl get pods failed or the namespace is empty)"
for p in $pods; do
  out_of_time && break
  name=${p#pod/}
  f="$dir/log-$name.txt"
  if k logs "$p" --all-containers --timestamps --since-time="$since" --limit-bytes="$max_bytes" > "$f" 2>/dev/null; then
    echo "--- $p: $(wc -l < "$f" | tr -d ' ') lines, all of them in $f"
    tail -n "$tail_n" "$f"
  else
    echo "--- $p: diag: no log"
    rm -f "$f"
  fi
  prev="$dir/log-$name.previous.txt"
  if k logs "$p" --all-containers --timestamps --since-time="$since" --limit-bytes="$max_bytes" --previous > "$prev" 2>/dev/null && [ -s "$prev" ]; then
    echo "--- $p: the previous container's log: $(wc -l < "$prev" | tr -d ' ') lines in $prev"
  else
    rm -f "$prev"
  fi
done
echo "::endgroup::"

echo "::group::diag: postgres sessions and locks"
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
cat "$pg"
echo "::endgroup::"
echo "diag: done in ${SECONDS}s; $(ls "$dir" | wc -l | tr -d ' ') files in $dir"
exit 0
