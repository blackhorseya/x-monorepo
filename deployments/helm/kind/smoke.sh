#!/usr/bin/env bash
# End-to-end through the installed release: rpc answers,
# the console serves the UI and proxies /api to the rpc.
#   smoke.sh <namespace> <release>
# Needs kubectl + curl. Exits non-zero (with the pods' state) when anything is off.
set -euo pipefail
# refuses to run outside run.sh (#444): HOME is the runner's scratch directory, which holds the marker
[ "${KIND_RUNNER_HOME:-}" = "${HOME:-}" ] && [ -f "${HOME:-/nonexistent}/.kind-runner" ] || { echo "$0: run it through its task (task helm-kind-up / helm-kind; CLUSTER=<name> picks the cluster): the task isolates it from ~/.kube/config (#444)" >&2; exit 2; }
ns=$1 release=$2 port=${SMOKE_PORT:-18080} cport=${SMOKE_CONSOLE_PORT:-18081}
api="http://127.0.0.1:$port/api"
console="http://127.0.0.1:$cport"

kubectl -n "$ns" port-forward "svc/$release-rpc" "$port:80" >/dev/null 2>&1 &
pf=$!
kubectl -n "$ns" port-forward "svc/$release-console" "$cport:80" >/dev/null 2>&1 &
pfc=$!
trap 'kill $pf $pfc 2>/dev/null || true' EXIT
for ((_try18 = 1; _try18 <= 30; _try18++)); do curl -fsS "$api/readyz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "$api/readyz" >/dev/null || { echo "smoke: /api/readyz never answered 200"; exit 1; }
version=$(curl -fsS "$api/version") || exit 1
echo "smoke: running $version"

# Console: the SPA for any route (TanStack Router handles /poison client-side), the rpc
# through nginx's /api proxy — what the browser does.
for ((_try57 = 1; _try57 <= 30; _try57++)); do curl -fsS "$console/healthz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "$console/poison" | grep -q "<title>x-monorepo console</title>" \
  || { echo "smoke: console did not serve index.html for /poison"; exit 1; }
curl -fsSI "$console/poison" | grep -qi '^content-security-policy: .*frame-ancestors' \
  || { echo "smoke: console index.html lacks the CSP header"; exit 1; }
curl -fsSI "$console/api/readyz" | grep -qi '^content-security-policy' \
  && { echo "smoke: console applied its CSP to a proxied /api response"; exit 1; }
curl -fsS "$console/api/readyz" >/dev/null || { echo "smoke: console /api proxy to the rpc failed"; exit 1; }
console_version=$(curl -fsS "$console/api/version") || exit 1
echo "smoke: console serves the UI and proxies /api → $console_version"

echo "smoke ok"
