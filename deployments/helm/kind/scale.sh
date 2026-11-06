#!/usr/bin/env bash
# Watches KEDA scale the worker on eventbus_consumer_lag: floods CreateOrder with k6 for a
# while, expects the worker Deployment to grow past minReplicas while the backlog is there,
# then expects the backlog to drain and the replicas to come back to minReplicas.
#   scale.sh <namespace> <release> [burst seconds]
# Needs kubectl, k6, curl, jq. Prints lag / replicas every few seconds so a failure is readable.
# shellcheck disable=SC2154   # `capture` (scripts/best-effort.sh) assigns the variables it is given, which shellcheck cannot see
set -euo pipefail
# refuses to run outside run.sh (#444): HOME is the runner's scratch directory, which holds the marker
[ "${KIND_RUNNER_HOME:-}" = "${HOME:-}" ] && [ -f "${HOME:-/nonexistent}/.kind-runner" ] || { echo "$0: run it through its task (task helm-kind-up / helm-kind / helm-kind-keda; CLUSTER=<name> picks the cluster): the task isolates it from ~/.kube/config (#444)" >&2; exit 2; }
. "$(dirname "$0")/../../../scripts/best-effort.sh"   # capture: a read that may fail while the system is not ready yet (#472)
abs_dir_of() { local p=${1%/*}; [ "$p" != "$1" ] || p=.; ( cd "$p${2:+/$2}" && pwd ); }   # the absolute directory of a script path (#472)
ns=$1 release=$2 burst=${3:-45}
rpc_port=${SCALE_RPC_PORT:-18082} prom_port=${SCALE_PROM_PORT:-19090}
deploy="deploy/$release-worker"

kubectl -n "$ns" port-forward "svc/$release-rpc" "$rpc_port:80" >/dev/null 2>&1 & pf1=$!
kubectl -n "$ns" port-forward svc/prometheus "$prom_port:9090" >/dev/null 2>&1 & pf2=$!
k6pid=""
trap 'kill $pf1 $pf2 $k6pid 2>/dev/null || true' EXIT
for ((_try18 = 1; _try18 <= 30; _try18++)); do curl -fsS "http://127.0.0.1:$rpc_port/api/readyz" >/dev/null 2>&1 && curl -fsS "http://127.0.0.1:$prom_port/-/ready" >/dev/null 2>&1 && break; sleep 1; done

group=$(kubectl -n "$ns" get scaledobject "$release-worker" -o jsonpath='{.spec.triggers[0].metadata.query}') || exit $?
lag() { curl -fsS "http://127.0.0.1:$prom_port/api/v1/query" --data-urlencode "query=$group" | jq -r '.data.result[0].value[1] // "none"'; }
replicas() { kubectl -n "$ns" get "$deploy" -o jsonpath='{.status.replicas}'; }
min=$(kubectl -n "$ns" get scaledobject "$release-worker" -o jsonpath='{.spec.minReplicaCount}') || exit $?

# The ScaledObject must be live before the burst: KEDA created the HPA and can query Prometheus.
for ((_try26 = 1; _try26 <= 30; _try26++)); do
  capture ready ready_rc kubectl -n "$ns" get scaledobject "$release-worker" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
  [ "$ready_rc" = 0 ] && [ "$ready" = True ] && break   # a failed read is "not ready yet": the loop retries
  sleep 2
done
kubectl -n "$ns" get scaledobject "$release-worker"
# The series shows up only after the worker's first metric export (OTEL_METRIC_EXPORT_INTERVAL)
# has been scraped by Prometheus: right after a fresh install that is a few tens of seconds.
for ((_try33 = 1; _try33 <= 60; _try33++)); do capture lag_now lag_rc lag; [ "$lag_rc" = 0 ] && [ "$lag_now" != none ] && break; sleep 2; done   # a failed read is "not yet"
lag_now=$(lag) || { echo "scale: cannot read Prometheus for: $group"; exit 1; }
[ "$lag_now" != none ] || { echo "scale: Prometheus has no series for: $group"; kubectl -n "$ns" logs deploy/otel-collector --tail=20; exit 1; }
# A previous burst may still be scaling down: start from the floor or the "scaled out" check is void.
for ((_try36 = 1; _try36 <= 120; _try36++)); do capture reps reps_rc replicas; [ "$reps_rc" = 0 ] && [ "$reps" = "$min" ] && break; sleep 2; done   # a failed read is "not yet"
reps=$(replicas) || { echo "scale: cannot read the worker's replicas"; exit 1; }
[ "$reps" = "$min" ] || { echo "scale: worker not at min ($min) before the burst: $reps"; exit 1; }
lag_start=$(lag) || { echo "scale: cannot read Prometheus for: $group"; exit 1; }
echo "scale: start lag=$lag_start replicas=$reps (min $min)"

k6dir=$(abs_dir_of "$0" ../../../tests/k6) || exit 1
k6 run --quiet --no-thresholds "$k6dir/orders.js" -e "BASE_URL=http://127.0.0.1:$rpc_port" -e VUS=30 -e "DURATION=${burst}s" -e THINK=0 -e "RUN_ID=scale-$$" >"${TMPDIR:-/tmp}/k6-scale.log" 2>&1 & k6pid=$!

up=""
for ((i = 1; i <= $((burst + 60)); i++)); do
  r=$(replicas) || exit $?; l=$(lag) || exit $?
  [ $((i % 5)) = 0 ] && echo "scale: t=${i}s lag=$l replicas=$r"
  if [ "$r" -gt "$min" ]; then up=$r; echo "scale: scaled out to $r replicas at t=${i}s (lag $l)"; break; fi
  sleep 1
done
[ -n "$up" ] || { echo "scale: worker never went past $min replicas"; kubectl -n "$ns" describe hpa; exit 1; }
wait $k6pid || { echo "scale: k6 failed:"; tail -30 "${TMPDIR:-/tmp}/k6-scale.log"; exit 1; }
k6pid=""

# Backlog drained → HPA brings the worker back down (behavior.scaleDown + cooldown from values).
for ((i = 1; i <= 240; i++)); do
  r=$(replicas) || exit $?; l=$(lag) || exit $?
  [ $((i % 10)) = 0 ] && echo "scale: drain t=${i}s lag=$l replicas=$r"
  if [ "$r" = "$min" ] && [ "$l" = 0 ]; then echo "scale ok: peaked at $up replicas, drained, back to $min"; exit 0; fi
  sleep 1
done
# the verdict is already "failed": the numbers below are only the report, so a failed read shows as empty text and does not hide that
capture reps_end reps_end_rc replicas
capture lag_end lag_end_rc lag
echo "scale: still at $reps_end replicas, lag $lag_end after 240s"
kubectl -n "$ns" describe hpa
exit 1
