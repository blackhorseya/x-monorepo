#!/usr/bin/env bash
# catalog-rpc must not come up on a Secret that cannot make it durable, and rpc must not come up on one that lacks
# its copy of the key. Three cases, each installed for
# real over the running release (helm upgrade, no --wait) and watched on the new pod:
#   1. the Secret has no APP_POSTGRES_URL   → CreateContainerConfigError (secretKeyRef optional: false)
#   2. APP_POSTGRES_URL is present but ""   → the container starts, then exits non-zero and never gets
#      Ready (--require-durable); its log names the flag and carries no secret
#   3. the Secret has no APP_CATALOG_API_KEY → the RPC pod does not start (CreateContainerConfigError): rpc
#      presents that key to catalog-rpc and reads it by secretKeyRef optional: false
# The release is restored (helm upgrade --wait) and the Secrets deleted on the way out, whatever happens.
#   negative.sh <namespace> <release> <chart-dir> <values-file>
# shellcheck disable=SC2154   # `capture` (scripts/best-effort.sh) assigns the variables it is given, which shellcheck cannot see
set -euo pipefail
. "$(dirname "$0")/../../../scripts/best-effort.sh"   # best_effort: diagnostics / cleanup / wait-gone; restore_step / restore_exit: restores (#462)
# refuses to run outside run.sh (#444): HOME is the runner's scratch directory, which holds the marker
[ "${KIND_RUNNER_HOME:-}" = "${HOME:-}" ] && [ -f "${HOME:-/nonexistent}/.kind-runner" ] || { echo "$0: run it through its task (task helm-kind-up / helm-kind / helm-kind-keda; CLUSTER=<name> picks the cluster): the task isolates it from ~/.kube/config (#444)" >&2; exit 2; }
ns=$1 release=$2 chart=$3 values=$4
sel="app.kubernetes.io/component=catalog-rpc"
key="kind-neg-key-0123456789abcd"

restore() {
  restore_step kubectl -n "$ns" delete secret catalog-rpc-no-url catalog-rpc-empty-url catalog-rpc-no-rpc-key --ignore-not-found >/dev/null   # restore: a failure fails the run (restore_exit), not best effort (#462)
  restore_step helm upgrade --install "$release" "$chart" -n "$ns" -f "$values" --wait --timeout 3m >/dev/null
}
on_exit() { local rc=$?; restore; restore_exit "$rc"; }
trap on_exit EXIT

upgrade_with() { # <secret name>
  helm upgrade --install "$release" "$chart" -n "$ns" -f "$values" \
    --set-string secrets.catalogRpc.apiKey= --set-string "secrets.catalogRpc.existingSecret=$1" >/dev/null
}

# poll <seconds> <jq filter over the catalog-rpc pods list that must print "yes">
poll_read() { kubectl -n "$ns" get pods -l "$sel" -o json | jq -r "$1"; }
poll() {
  local ans rc
  for ((_try32 = 1; _try32 <= "$1"; _try32++)); do
    capture ans rc poll_read "$2" || return 1
    [ "$rc" = 0 ] && [ "$ans" = yes ] && return 0   # a failed read is "not yet": the loop retries, running out of tries is the failure
    sleep 1
  done
  return 1
}

kubectl -n "$ns" create secret generic catalog-rpc-no-url --from-literal=APP_AUTH_API_KEYS="rpc=$key:admin" --from-literal=APP_CATALOG_API_KEY="$key" >/dev/null
upgrade_with catalog-rpc-no-url
poll 90 'if any(.items[]; .status.containerStatuses[0].state.waiting.reason == "CreateContainerConfigError") then "yes" else "no" end' \
  || { echo "negative: a Secret without APP_POSTGRES_URL did not stop the catalog-rpc pod (CreateContainerConfigError)"; kubectl -n "$ns" get pods -l "$sel"; exit 1; }
echo "negative ok: a Secret without APP_POSTGRES_URL keeps the catalog-rpc pod from starting (CreateContainerConfigError)"

kubectl -n "$ns" create secret generic catalog-rpc-empty-url \
  --from-literal=APP_AUTH_API_KEYS="rpc=$key:admin" --from-literal=APP_CATALOG_API_KEY="$key" --from-literal=APP_POSTGRES_URL= >/dev/null
upgrade_with catalog-rpc-empty-url
# a pod of the new ReplicaSet that ran and exited non-zero, and is not Ready
poll 120 'if any(.items[]; (.status.containerStatuses[0].lastState.terminated.exitCode // 0) != 0 and (.status.containerStatuses[0].ready | not)) then "yes" else "no" end' \
  || { echo "negative: an empty APP_POSTGRES_URL did not make catalog-rpc exit (--require-durable)"; kubectl -n "$ns" get pods -l "$sel"; exit 1; }
# The refusal, read from evidence that outlives the container: terminated.message in the Pod status
# (the chart sets terminationMessagePolicy: FallbackToLogsOnError), else `kubectl logs` (previous, then
# current). Bounded: NEGATIVE_TRIES (default 60) tries, NEGATIVE_SLEEP (default 2) seconds apart. A failed read is a retry, never "the log": only text
# that a successful read returned is asserted on.
refusal=""
for ((_try56 = 1; _try56 <= "${NEGATIVE_TRIES:-60}"; _try56++)); do
  # a failed read (API hiccup, unparsable output) is a retry like any other: set -e must not end the script
  capture pods pods_rc kubectl -n "$ns" get pods -l "$sel" -o json 2>/dev/null || exit 1
  if [ "$pods_rc" != 0 ]; then sleep "${NEGATIVE_SLEEP:-2}"; continue; fi
  capture bad bad_rc jq -r '[.items[] | select((.status.containerStatuses[0].lastState.terminated.exitCode // 0) != 0) | .metadata.name][0] // empty' <<<"$pods" 2>/dev/null || exit 1
  if [ "$bad_rc" != 0 ]; then sleep "${NEGATIVE_SLEEP:-2}"; continue; fi
  if [ -n "$bad" ]; then
    capture msg msg_rc jq -r --arg n "$bad" '.items[] | select(.metadata.name == $n) | .status.containerStatuses[0].lastState.terminated.message // empty' <<<"$pods" 2>/dev/null || exit 1
    if [ "$msg_rc" != 0 ]; then sleep "${NEGATIVE_SLEEP:-2}"; continue; fi   # a failed read is a retry, never an empty message
    if grep -q 'require-durable' <<<"$msg"; then refusal=$msg; break; fi
    for prev in --previous ""; do
      # shellcheck disable=SC2086
      capture out out_rc kubectl -n "$ns" logs "$bad" $prev 2>/dev/null || exit 1
      if [ "$out_rc" = 0 ] && grep -q 'require-durable' <<<"$out"; then refusal=$out; break 2; fi
    done
  fi
  sleep "${NEGATIVE_SLEEP:-2}"
done
[ -n "$refusal" ] || { echo "negative: no readable --require-durable refusal from the failing catalog-rpc pod (Pod status or logs) within 2 minutes"; kubectl -n "$ns" get pods -l "$sel"; exit 1; }
grep -q "$key" <<<"$refusal" && { echo "negative: the failing pod's refusal carries the service key"; exit 1; }
echo "negative ok: an empty APP_POSTGRES_URL makes catalog-rpc exit non-zero and never Ready (--require-durable)"

# 3. rpc's copy of the key is missing from the Secret catalog-rpc is otherwise happy with
pgurl=$(sed -n 's/^  postgresUrl: //p' "$values" | head -n 1)
[ -n "$pgurl" ] || { echo "negative: no secrets.postgresUrl in $values"; exit 1; }
kubectl -n "$ns" create secret generic catalog-rpc-no-rpc-key \
  --from-literal=APP_AUTH_API_KEYS="rpc=$key:admin" --from-literal=APP_POSTGRES_URL="$pgurl" >/dev/null
upgrade_with catalog-rpc-no-rpc-key
# bounded like every read here (NEGATIVE_TRIES x NEGATIVE_SLEEP, default 90 x 1s); a failed read is a retry
# rpc_config_error: ONE read of the rpc pods, "yes" when one waits on CreateContainerConfigError, "no" otherwise; fails when the read fails
rpc_config_error() { kubectl -n "$ns" get pods -l app.kubernetes.io/component=rpc -o json 2>/dev/null | jq -r 'if any(.items[]; .status.containerStatuses[0].state.waiting.reason == "CreateContainerConfigError") then "yes" else "no" end' 2>/dev/null; }
found=""
for ((_try82 = 1; _try82 <= "${NEGATIVE_RPC_TRIES:-90}"; _try82++)); do
  capture cfg cfg_rc rpc_config_error || exit 1
  if [ "$cfg_rc" = 0 ] && [ "$cfg" = yes ]; then found=yes; break; fi   # a failed read is "not yet": the loop retries, running out of tries fails below
  sleep "${NEGATIVE_RPC_SLEEP:-1}"
done
[ "$found" = yes ] || { echo "negative: a Secret without APP_CATALOG_API_KEY did not stop the rpc pod (CreateContainerConfigError)"; kubectl -n "$ns" get pods; exit 1; }
echo "negative ok: a Secret without APP_CATALOG_API_KEY keeps the rpc pod from starting (CreateContainerConfigError)"
