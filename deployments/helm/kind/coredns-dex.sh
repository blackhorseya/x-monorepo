#!/usr/bin/env bash
# Makes dex.localtest.me resolve to the in-cluster Service dex inside kind, so a pod sees the same
# issuer URL (http://dex.localtest.me:5556/dex) a browser does. On a public resolver
# *.localtest.me is 127.0.0.1, which inside a pod is the pod itself — hence the CoreDNS rewrite.
# Idempotent. Restarts CoreDNS only when the Corefile changed.
#   coredns-dex.sh <namespace-of-dex>
set -euo pipefail
ns=${1:?usage: coredns-dex.sh <namespace>}
rule="rewrite name exact dex.localtest.me dex.$ns.svc.cluster.local"

corefile=$(kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
if grep -qF -- "$rule" <<<"$corefile"; then
  echo "coredns: rewrite already present"
  exit 0
fi
if grep -q 'rewrite name exact dex.localtest.me' <<<"$corefile"; then
  corefile=$(grep -v 'rewrite name exact dex.localtest.me' <<<"$corefile")   # namespace changed
fi
# After `ready`, before `kubernetes`: the rewrite must run before the zone lookup.
new=$(awk -v rule="$rule" '{print} /^[[:space:]]*ready[[:space:]]*$/ && !done {print "    " rule; done=1}' <<<"$corefile")
grep -qF -- "$rule" <<<"$new" || { echo "coredns: no 'ready' line in the Corefile to anchor the rewrite" >&2; exit 1; }

kubectl -n kube-system patch configmap coredns --type merge \
  -p "$(jq -n --arg c "$new" '{data: {Corefile: $c}}')" >/dev/null
kubectl -n kube-system rollout restart deployment/coredns >/dev/null
kubectl -n kube-system rollout status deployment/coredns --timeout=2m
echo "coredns: dex.localtest.me → dex.$ns.svc.cluster.local"
