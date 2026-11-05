#!/usr/bin/env bash
# What `task helm-kind` runs instead of payment-upgrade.sh directly (#427, #429): the same arguments, plus a READ-ONLY proof
# that the upgrade drill left the home release's database alone.
#   payment-upgrade-guard.sh <home-namespace> <home-release> <chart-dir> <values-file> <flow-values-file>
# `helm-kind-up` reuses an existing kind cluster and its Postgres keeps its data between runs, so the home database may hold
# things that are not this test's. The drill itself runs in a namespace of its own and never touches it; this checks that, by
# taking a snapshot before and one after (every schema, every table with its exact row count) and requiring that it is not
# empty and did not change. It writes nothing, there or anywhere (every statement runs with default_transaction_read_only on).
# The exit code is the drill's own, or 1 when the snapshot is empty or differs.
# It reads no state from the environment (the drill it runs is the payment-upgrade.sh next to this file, nothing else) beyond
# what the tools read (PATH, KUBECONFIG, ...).
set -uo pipefail
# refuses to run outside run.sh (#444): HOME is the runner's scratch directory, which holds the marker
[ "${KIND_RUNNER_HOME:-}" = "${HOME:-}" ] && [ -f "${HOME:-/nonexistent}/.kind-runner" ] || { echo "$0: run it through its task (task helm-kind-up / helm-kind / helm-kind-keda; CLUSTER=<name> picks the cluster): the task isolates it from ~/.kube/config (#444)" >&2; exit 2; }
before="" after="" rc=0
home=$1
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
inner=$here/payment-upgrade.sh
q() { kubectl -n "$home" exec deploy/postgres -- psql -U app -d app -tAq -c "SET default_transaction_read_only = on; $1"; }

snapshot() {
  local t tables n
  q "SELECT 'schema ' || nspname FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema' ORDER BY 1" || return 1
  # each read on its own line, not inside `for … in $(q …)` or `echo "$(q …)"`: a q that prints part of its answer and then dies
  # would be a shorter snapshot (a missing table, a count of nothing) and before == after would still hold (#462)
  tables=$(q "SELECT format('%I.%I', table_schema, table_name) FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema !~ '^pg_' AND table_schema <> 'information_schema' ORDER BY 1") || return 1
  for t in $tables; do
    n=$(q "SELECT count(*) FROM $t") || return 1
    echo "table $t $n"
  done
}

before=$(snapshot) || { echo "guard: cannot read the home release's database" >&2; exit 1; }
[ "$(grep -c '^table ' <<<"$before")" -gt 0 ] || { echo "guard: the snapshot of the home release's database has no table: nothing would be proved" >&2; exit 1; }

bash "$inner" "$@"
rc=$?

after=$(snapshot) || { echo "guard: cannot read the home release's database after the drill" >&2; exit 1; }
if [ "$before" != "$after" ]; then
  echo "guard: the home release's database CHANGED during the drill:" >&2
  diff <(echo "$before") <(echo "$after") >&2
  exit 1
fi
echo "guard: the home release's database is unchanged ($(grep -c '^table ' <<<"$before") tables, every row count the same)"
exit "$rc"
