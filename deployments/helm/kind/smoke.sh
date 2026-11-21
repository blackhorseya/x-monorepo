#!/usr/bin/env bash
# End-to-end through the installed release: rpc answers.
#   smoke.sh <namespace> <release>
# Needs kubectl + curl. Exits non-zero (with the pods' state) when anything is off.
set -euo pipefail
# refuses to run outside run.sh (#444): HOME is the runner's scratch directory, which holds the marker
[ "${KIND_RUNNER_HOME:-}" = "${HOME:-}" ] && [ -f "${HOME:-/nonexistent}/.kind-runner" ] || { echo "$0: run it through its task (task helm-kind-up / helm-kind; CLUSTER=<name> picks the cluster): the task isolates it from ~/.kube/config (#444)" >&2; exit 2; }
ns=$1 release=$2 port=${SMOKE_PORT:-18080}
api="http://127.0.0.1:$port/api"

kubectl -n "$ns" port-forward "svc/$release-rpc" "$port:80" >/dev/null 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
for ((_try18 = 1; _try18 <= 30; _try18++)); do curl -fsS "$api/readyz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "$api/readyz" >/dev/null || { echo "smoke: /api/readyz never answered 200"; exit 1; }
version=$(curl -fsS "$api/version") || exit 1
echo "smoke: running $version"

echo "smoke ok"
