#!/usr/bin/env bash
# What `task helm-kind` runs instead of payment-upgrade.sh directly (#427, #429): the same arguments, plus the proof
# that the upgrade drill leaves the release's own database alone. `helm-kind-up` reuses an existing kind cluster and its
# Postgres keeps its data between runs, so "app" may hold things that are not this test's.
#   payment-upgrade-guard.sh <namespace> <release> <chart-dir> <values-file> <flow-values-file>
# Before the drill a schema with one row is written to "app"; after it (success OR failure) the row must still be there
# and no per-run database (upgrade_*) may be left. The sentinel schema is removed at the end. The exit code is the
# drill's own, or 1 when the guard itself finds damage.
# UPGRADE_SCRIPT (default: payment-upgrade.sh next to this file) lets the test run a mutated copy of the drill.
set -uo pipefail
ns=$1
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
inner=${UPGRADE_SCRIPT:-$here/payment-upgrade.sh}
schema=upgrade_guard_sentinel
value="keep-$(date +%s)-$RANDOM"
adb() { kubectl -n "$ns" exec deploy/postgres -- psql -U app -d app -tAq -c "$1"; }

adb "SET client_min_messages = warning; DROP SCHEMA IF EXISTS $schema CASCADE; CREATE SCHEMA $schema; CREATE TABLE $schema.keep (v text); INSERT INTO $schema.keep VALUES ('$value')" >/dev/null \
  || { echo "guard: could not write the sentinel to the release's database" >&2; exit 1; }

bash "$inner" "$@"
rc=$?

rc_guard=0
got=$(adb "SELECT v FROM $schema.keep" 2>/dev/null)
if [ "$got" != "$value" ]; then
  echo "guard: the sentinel in the release's database is LOST or changed (got '$got'): the upgrade drill modified what is not its own" >&2
  rc_guard=1
fi
left=$(adb "SELECT string_agg(datname, ',') FROM pg_database WHERE datname LIKE 'upgrade\\_%'" 2>/dev/null)
if [ -n "$left" ]; then
  echo "guard: per-run database(s) left behind: $left" >&2
  rc_guard=1
fi
[ "$rc_guard" = 0 ] && echo "guard: sentinel intact, no per-run database left"
adb "SET client_min_messages = warning; DROP SCHEMA IF EXISTS $schema CASCADE" >/dev/null 2>&1 || true
[ "$rc_guard" = 0 ] || exit 1
exit "$rc"
