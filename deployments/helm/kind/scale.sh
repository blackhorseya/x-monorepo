#!/usr/bin/env bash
# Watches KEDA scale the worker on eventbus_consumer_lag: floods CreateOrder with k6 for a
# while, expects the worker Deployment to grow past minReplicas while the backlog is there,
# then expects the backlog to drain and the replicas to come back to minReplicas.
#   scale.sh <namespace> <release> [burst seconds]
# Needs kubectl, k6, curl, jq. Prints lag / replicas every few seconds so a failure is readable.
set -euo pipefail
ns=$1 release=$2 burst=${3:-45}
rpc_port=${SCALE_RPC_PORT:-18082} prom_port=${SCALE_PROM_PORT:-19090}
deploy="deploy/$release-worker"

kubectl -n "$ns" port-forward "svc/$release-rpc" "$rpc_port:80" >/dev/null 2>&1 & pf1=$!
kubectl -n "$ns" port-forward svc/prometheus "$prom_port:9090" >/dev/null 2>&1 & pf2=$!
k6pid=""
trap 'kill $pf1 $pf2 $k6pid 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -fsS "http://127.0.0.1:$rpc_port/api/readyz" >/dev/null 2>&1 && curl -fsS "http://127.0.0.1:$prom_port/-/ready" >/dev/null 2>&1 && break; sleep 1; done

group=$(kubectl -n "$ns" get scaledobject "$release-worker" -o jsonpath='{.spec.triggers[0].metadata.query}')
lag() { curl -fsS "http://127.0.0.1:$prom_port/api/v1/query" --data-urlencode "query=$group" | jq -r '.data.result[0].value[1] // "none"'; }
replicas() { kubectl -n "$ns" get "$deploy" -o jsonpath='{.status.replicas}'; }
min=$(kubectl -n "$ns" get scaledobject "$release-worker" -o jsonpath='{.spec.minReplicaCount}')

# The ScaledObject must be live before the burst: KEDA created the HPA and can query Prometheus.
for _ in $(seq 1 30); do
  [ "$(kubectl -n "$ns" get scaledobject "$release-worker" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ] && break
  sleep 2
done
kubectl -n "$ns" get scaledobject "$release-worker"
# The series shows up only after the worker's first metric export (OTEL_METRIC_EXPORT_INTERVAL)
# has been scraped by Prometheus: right after a fresh install that is a few tens of seconds.
for _ in $(seq 1 60); do [ "$(lag)" != none ] && break; sleep 2; done
[ "$(lag)" != none ] || { echo "scale: Prometheus has no series for: $group"; kubectl -n "$ns" logs deploy/otel-collector --tail=20; exit 1; }
# A previous burst may still be scaling down: start from the floor or the "scaled out" check is void.
for _ in $(seq 1 120); do [ "$(replicas)" = "$min" ] && break; sleep 2; done
[ "$(replicas)" = "$min" ] || { echo "scale: worker not at min ($min) before the burst: $(replicas)"; exit 1; }
echo "scale: start lag=$(lag) replicas=$(replicas) (min $min)"

k6 run --quiet --no-thresholds "$(dirname "$0")/../../../tests/k6/orders.js" -e "BASE_URL=http://127.0.0.1:$rpc_port" -e VUS=30 -e "DURATION=${burst}s" -e THINK=0 -e "RUN_ID=scale-$$" >"${TMPDIR:-/tmp}/k6-scale.log" 2>&1 & k6pid=$!

up=""
for i in $(seq 1 $((burst + 60))); do
  r=$(replicas); l=$(lag)
  [ $((i % 5)) = 0 ] && echo "scale: t=${i}s lag=$l replicas=$r"
  if [ "$r" -gt "$min" ]; then up=$r; echo "scale: scaled out to $r replicas at t=${i}s (lag $l)"; break; fi
  sleep 1
done
[ -n "$up" ] || { echo "scale: worker never went past $min replicas"; kubectl -n "$ns" describe hpa; exit 1; }
wait $k6pid || { echo "scale: k6 failed:"; tail -30 "${TMPDIR:-/tmp}/k6-scale.log"; exit 1; }
k6pid=""

# Backlog drained → HPA brings the worker back down (behavior.scaleDown + cooldown from values).
for i in $(seq 1 240); do
  r=$(replicas); l=$(lag)
  [ $((i % 10)) = 0 ] && echo "scale: drain t=${i}s lag=$l replicas=$r"
  if [ "$r" = "$min" ] && [ "$l" = 0 ]; then echo "scale ok: peaked at $up replicas, drained, back to $min"; exit 0; fi
  sleep 1
done
echo "scale: still at $(replicas) replicas, lag $(lag) after 240s"
kubectl -n "$ns" describe hpa
exit 1
