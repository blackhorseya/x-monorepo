#!/usr/bin/env bash
# What a failed kind run leaves behind for finding the cause (#416):
#   diag.sh <namespace> <since: RFC 3339 UTC time the run started>
# Prints to stdout, in ::group:: blocks (collapsed in the Actions log):
#   1  pods and events
#   2  the WHOLE log of every pod since <since> (no --tail: a tail of 200 lines per pod is a few seconds of a
#      busy rpc), with timestamps, and the previous container's log where there is one
#   3  Postgres: every session (state, wait event, how long in its transaction, the query text without
#      parameters), who blocks whom, every lock, and connections against max_connections
# It is called BEFORE the run restores the release: `helm upgrade` replaces the pods, and with them the
# logs this reads. Best effort: it never fails and never waits long, so it cannot hide the failure it
# reports on. Needs kubectl.
set -uo pipefail
ns=${1:?usage: diag.sh <namespace> <since RFC3339>} since=${2:?usage: diag.sh <namespace> <since RFC3339>}
k() { kubectl --request-timeout=30s -n "$ns" "$@"; }

echo "::group::diag: pods and events"
k get pods -o wide || true
k get events --sort-by=.lastTimestamp || true
echo "::endgroup::"

echo "::group::diag: pod logs since $since"
# one name per line; a failed list is "no pods", and the group says so
pods=$(k get pods -o name 2>/dev/null) || pods=""
[ -n "$pods" ] || echo "diag: no pod list (kubectl get pods failed or the namespace is empty)"
for p in $pods; do
  echo "--- $p"
  k logs "$p" --all-containers --timestamps --since-time="$since" || echo "diag: no log for $p"
  k logs "$p" --all-containers --timestamps --since-time="$since" --previous 2>/dev/null | sed 's/^/previous: /' || true
done
echo "::endgroup::"

echo "::group::diag: postgres sessions and locks"
sql() { # <title> <statement>: read-only, bounded to 10 s on the server
  echo "--- $1"
  k exec deploy/postgres -- psql -U app -d app -X -q -P pager=off -v ON_ERROR_STOP=1 \
    -c "SET statement_timeout = '10s'" -c "SET default_transaction_read_only = on" -c "$2" || echo "diag: '$1' failed"
}
sql "sessions (pg_stat_activity)" "SELECT pid, application_name, state, wait_event_type, wait_event, now() - xact_start AS in_xact, now() - query_start AS in_query, left(regexp_replace(query, '\s+', ' ', 'g'), 160) AS query FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() ORDER BY xact_start NULLS LAST, pid"
sql "blocked sessions (pg_blocking_pids)" "SELECT a.pid, pg_blocking_pids(a.pid) AS blocked_by, a.wait_event_type, a.wait_event, now() - a.query_start AS waiting_for, left(regexp_replace(a.query, '\s+', ' ', 'g'), 160) AS query FROM pg_stat_activity a WHERE cardinality(pg_blocking_pids(a.pid)) > 0"
sql "locks (pg_locks)" "SELECT l.pid, l.locktype, l.mode, l.granted, c.relname FROM pg_locks l LEFT JOIN pg_class c ON c.oid = l.relation WHERE l.pid <> pg_backend_pid() ORDER BY l.granted, l.pid, c.relname"
sql "connections against max_connections" "SELECT count(*) AS connections, current_setting('max_connections') AS max_connections FROM pg_stat_activity"
echo "::endgroup::"
exit 0
