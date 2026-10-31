#!/usr/bin/env bash
# catalog-rpc must not come up on a Secret that cannot make it durable. Two cases, each installed for
# real over the running release (helm upgrade, no --wait) and watched on the new pod:
#   1. the Secret has no APP_POSTGRES_URL   → CreateContainerConfigError (secretKeyRef optional: false)
#   2. APP_POSTGRES_URL is present but ""   → the container starts, then exits non-zero and never gets
#      Ready (--require-durable); its log names the flag and carries no secret
# The release is restored (helm upgrade --wait) and the Secrets deleted on the way out, whatever happens.
#   negative.sh <namespace> <release> <chart-dir> <values-file>
set -euo pipefail
ns=$1 release=$2 chart=$3 values=$4
sel="app.kubernetes.io/component=catalog-rpc"
key="kind-neg-key-0123456789abcd"

restore() {
  kubectl -n "$ns" delete secret catalog-rpc-no-url catalog-rpc-empty-url --ignore-not-found >/dev/null 2>&1 || true
  helm upgrade --install "$release" "$chart" -n "$ns" -f "$values" --wait --timeout 3m >/dev/null
}
trap restore EXIT

upgrade_with() { # <secret name>
  helm upgrade --install "$release" "$chart" -n "$ns" -f "$values" \
    --set-string secrets.catalogRpc.apiKey= --set-string "secrets.catalogRpc.existingSecret=$1" >/dev/null
}

# poll <seconds> <jq filter over the catalog-rpc pods list that must print "yes">
poll() {
  for _ in $(seq 1 "$1"); do
    [ "$(kubectl -n "$ns" get pods -l "$sel" -o json | jq -r "$2")" = yes ] && return 0
    sleep 1
  done
  return 1
}

kubectl -n "$ns" create secret generic catalog-rpc-no-url --from-literal=APP_AUTH_API_KEYS="rpc=$key:admin" >/dev/null
upgrade_with catalog-rpc-no-url
poll 90 'if any(.items[]; .status.containerStatuses[0].state.waiting.reason == "CreateContainerConfigError") then "yes" else "no" end' \
  || { echo "negative: a Secret without APP_POSTGRES_URL did not stop the catalog-rpc pod (CreateContainerConfigError)"; kubectl -n "$ns" get pods -l "$sel"; exit 1; }
echo "negative ok: a Secret without APP_POSTGRES_URL keeps the catalog-rpc pod from starting (CreateContainerConfigError)"

kubectl -n "$ns" create secret generic catalog-rpc-empty-url \
  --from-literal=APP_AUTH_API_KEYS="rpc=$key:admin" --from-literal=APP_POSTGRES_URL= >/dev/null
upgrade_with catalog-rpc-empty-url
# a pod of the new ReplicaSet that ran and exited non-zero, and is not Ready
poll 120 'if any(.items[]; (.status.containerStatuses[0].lastState.terminated.exitCode // 0) != 0 and (.status.containerStatuses[0].ready | not)) then "yes" else "no" end' \
  || { echo "negative: an empty APP_POSTGRES_URL did not make catalog-rpc exit (--require-durable)"; kubectl -n "$ns" get pods -l "$sel"; exit 1; }
# The refusal, read from evidence that outlives the container: terminated.message in the Pod status
# (the chart sets terminationMessagePolicy: FallbackToLogsOnError), else `kubectl logs` (previous, then
# current). Bounded: NEGATIVE_TRIES (default 60) tries, NEGATIVE_SLEEP (default 2) seconds apart. A failed read is a retry, never "the log": only text
# that a successful read returned is asserted on.
refusal=""
for _ in $(seq 1 "${NEGATIVE_TRIES:-60}"); do
  # a failed read (API hiccup, unparsable output) is a retry like any other: set -e must not end the script
  pods=$(kubectl -n "$ns" get pods -l "$sel" -o json 2>/dev/null) || { sleep "${NEGATIVE_SLEEP:-2}"; continue; }
  bad=$(jq -r '[.items[] | select((.status.containerStatuses[0].lastState.terminated.exitCode // 0) != 0) | .metadata.name][0] // empty' <<<"$pods" 2>/dev/null) || { sleep "${NEGATIVE_SLEEP:-2}"; continue; }
  if [ -n "$bad" ]; then
    msg=$(jq -r --arg n "$bad" '.items[] | select(.metadata.name == $n) | .status.containerStatuses[0].lastState.terminated.message // empty' <<<"$pods" 2>/dev/null) || msg=""
    if grep -q 'require-durable' <<<"$msg"; then refusal=$msg; break; fi
    for prev in --previous ""; do
      # shellcheck disable=SC2086
      if out=$(kubectl -n "$ns" logs "$bad" $prev 2>/dev/null) && grep -q 'require-durable' <<<"$out"; then refusal=$out; break 2; fi
    done
  fi
  sleep "${NEGATIVE_SLEEP:-2}"
done
[ -n "$refusal" ] || { echo "negative: no readable --require-durable refusal from the failing catalog-rpc pod (Pod status or logs) within 2 minutes"; kubectl -n "$ns" get pods -l "$sel"; exit 1; }
grep -q "$key" <<<"$refusal" && { echo "negative: the failing pod's refusal carries the service key"; exit 1; }
echo "negative ok: an empty APP_POSTGRES_URL makes catalog-rpc exit non-zero and never Ready (--require-durable)"
