#!/usr/bin/env bash
# Makes dex.localtest.me resolve to the in-cluster Service dex inside kind, so a pod sees the same
# issuer URL (http://dex.localtest.me:5556/dex) a browser does. On a public resolver
# *.localtest.me is 127.0.0.1, which inside a pod is the pod itself — hence the CoreDNS rewrite.
# Idempotent. Restarts CoreDNS only when the Corefile changed.
#   coredns-dex.sh <namespace-of-dex>
#
# Only a complete directive `rewrite name exact dex.localtest.me <target>` is ours: other rewrites
# (including names that merely start with dex.localtest.me) and comments are left alone, and a
# commented-out copy of the rule is not "already present".
# scripts/test.sh drives this file against a stub kubectl.
set -euo pipefail
ns=${1:?usage: coredns-dex.sh <namespace>}
target="dex.$ns.svc.cluster.local"

corefile=$(kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
# Drop our directive wherever it is (a stale target after a namespace change, or a copy that is
# not where we want it), then put exactly one after `ready`, before `kubernetes`: the rewrite
# must run before the zone lookup.
new=$(awk -v target="$target" '
  $1 == "rewrite" && $2 == "name" && $3 == "exact" && $4 == "dex.localtest.me" && NF == 5 { next }
  { print }
  $1 == "ready" && NF == 1 && !done { print "    rewrite name exact dex.localtest.me " target; done = 1 }
' <<<"$corefile")
grep -qxF "    rewrite name exact dex.localtest.me $target" <<<"$new" \
  || { echo "coredns: no 'ready' line in the Corefile to anchor the rewrite" >&2; exit 1; }

if [ "$new" = "$corefile" ]; then
  echo "coredns: rewrite already present"
  exit 0
fi

kubectl -n kube-system patch configmap coredns --type merge \
  -p "$(jq -n --arg c "$new" '{data: {Corefile: $c}}')" >/dev/null
kubectl -n kube-system rollout restart deployment/coredns >/dev/null
kubectl -n kube-system rollout status deployment/coredns --timeout=2m
echo "coredns: dex.localtest.me → $target"
