#!/usr/bin/env bash
# negative.sh's reading of the refusal, driven through the real script with fake kubectl / helm: no cluster.
# Two evidence sources, four cells each (a source that is added later gets its own four):
#
#   source        success                 empty value              fails once, then recovers        fails for good
#   Pod status    S1 refusal in status    S2 status message ""     S3 `get pods` fails once         S4 `get pods` fails from the 3rd call
#   kubectl logs  L1 logs answer at once  L2 logs exit 0, no text  L3 logs fail twice, then answer  L4 logs always fail
#
# plus the rpc-pod phase (R: the Secret lacks rpc's copy of the key → the rpc pod must not start) and the two ways a reader lies: T `kubectl logs` prints the tool's own error and exits 0 (never "the log"),
# K the refusal carries the service key (must fail). Every run, passing or failing, also asserts what the
# script owes whatever happens: the release is restored last (`helm upgrade --wait`), both test Secrets are
# deleted, and the reads are bounded (NEGATIVE_TRIES), so a permanent failure exits instead of hanging.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
key="kind-neg-key-0123456789abcd"
# the values file negative.sh reads the Postgres URL from (kind/values.yaml has the same shape)
printf 'secrets:\n  postgresUrl: postgres://app:app@postgres:5432/app?sslmode=disable\n' > "$tmp/values.yaml"

# fake helm: succeed. fake kubectl: serve the pod JSON in $POD_JSON, logs per $LOG_MODE.
cat > "$tmp/bin/helm" <<'H'
#!/usr/bin/env bash
echo "$*" >> "$HLOG"
exit 0
H
cat > "$tmp/bin/kubectl" <<'K'
#!/usr/bin/env bash
args="$*"
echo "$args" >> "$KLOG"
case $args in
  *"create secret"*|*"delete secret"*) exit 0 ;;
  *"get pods"*"component=rpc"*)
    # negative.sh's rpc phase: RPC_PODS=broken → an rpc pod stuck in CreateContainerConfigError (the expected
    # result); healthy → a Running pod, i.e. the Secret without the key did NOT stop the rpc
    if [ "${RPC_PODS:-broken}" = broken ]; then
      echo '{"items":[{"metadata":{"name":"rpc-1"},"status":{"containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CreateContainerConfigError"}}}]}}]}'
    else
      echo '{"items":[{"metadata":{"name":"rpc-1"},"status":{"containerStatuses":[{"ready":true,"state":{"running":{}}}]}}]}'
    fi ;;
  *"get pods"*)
    pn=$(cat "$tmp_pods" 2>/dev/null || echo 0); pn=$((pn+1)); echo "$pn" > "$tmp_pods"
    if [ "${GETPODS_FAIL_AT:-0}" = "$pn" ] || { [ "${GETPODS_FAIL_FROM:-0}" -gt 0 ] && [ "$pn" -ge "${GETPODS_FAIL_FROM:-0}" ]; }; then echo "error: the server is currently unable to handle the request" >&2; exit 1; fi
    # the first scenario (missing key) needs a CreateContainerConfigError pod; the second a terminated one
    if [ "${PHASE_FILE:-}" ] && [ ! -e "$PHASE_FILE" ]; then
      touch "$PHASE_FILE"
      echo '{"items":[{"metadata":{"name":"p1"},"status":{"containerStatuses":[{"state":{"waiting":{"reason":"CreateContainerConfigError"}}}]}}]}'
    else
      cat "$POD_JSON"
    fi ;;
  *"logs"*)
    n=$(cat "$tmp_count" 2>/dev/null || echo 0); echo $((n+1)) > "$tmp_count"
    case $LOG_MODE in
      fail)       echo "unable to retrieve container logs for containerd://abc" >&2; exit 1 ;;
      flaky)      if [ "$n" -lt 2 ]; then echo "unable to retrieve container logs" >&2; exit 1; fi; echo "$LOG_TEXT" ;;
      empty)      exit 0 ;;
      tool-error) echo "unable to retrieve container logs for containerd://abc"; exit 0 ;;
      ok)         echo "$LOG_TEXT" ;;
    esac ;;
esac
K
chmod +x "$tmp/bin/helm" "$tmp/bin/kubectl"

pod() { # <terminated.message>
  jq -n --arg m "$1" '{items:[{metadata:{name:"p1"},status:{containerStatuses:[{ready:false,state:{waiting:{reason:"CrashLoopBackOff"}},lastState:{terminated:{exitCode:1,message:$m}}}]}}]}'
}

run() { # <name> <want exit: 0|1> <want text in output> ; env set by caller
  : > "$tmp/helm.log"; : > "$tmp/kubectl.log"
  out=$(PATH="$tmp/bin:$PATH" PHASE_FILE="$tmp/phase" tmp_count="$tmp/count" tmp_pods="$tmp/pods.count" HLOG="$tmp/helm.log" KLOG="$tmp/kubectl.log" \
    POD_JSON="$tmp/pods.json" NEGATIVE_TRIES=4 NEGATIVE_SLEEP=0 NEGATIVE_RPC_TRIES=3 NEGATIVE_RPC_SLEEP=0 "$here/negative.sh" ns rel chart "${VALUES_FILE:-$tmp/values.yaml}" 2>&1) && got=0 || got=1
  pods_calls=$(cat "$tmp/pods.count" 2>/dev/null || echo 0)
  rm -f "$tmp/phase" "$tmp/count" "$tmp/pods.count"
  [ "$got" = "$2" ] || { echo "negative-test: $1: exit $got, want $2"; echo "$out"; exit 1; }
  case $out in *"$3"*) ;; *) echo "negative-test: $1: output lacks '$3'"; echo "$out"; exit 1 ;; esac
  # owed whatever the outcome: restore last, Secrets deleted, reads bounded
  tail -n 1 "$tmp/helm.log" | grep -q -- '--wait' || { echo "negative-test: $1: the release was not restored last (helm upgrade --wait)"; cat "$tmp/helm.log"; exit 1; }
  grep -q 'delete secret catalog-rpc-no-url catalog-rpc-empty-url catalog-rpc-no-rpc-key' "$tmp/kubectl.log" || { echo "negative-test: $1: the test Secrets were not deleted"; exit 1; }
  [ "$pods_calls" -le 12 ] || { echo "negative-test: $1: $pods_calls pod reads, the loop is not bounded"; exit 1; }
  echo "negative-test ok: $1"
}

REF='level=ERROR msg="catalog-rpc exited" error="catalog-rpc: --require-durable: postgres.url is empty"'
OK="negative ok: an empty APP_POSTGRES_URL"
NOREAD="no readable --require-durable refusal"

# ── Pod status ───────────────────────────────────────────────────────────────
pod "$REF" > "$tmp/pods.json"
LOG_MODE=fail run "S1 status: success" 0 "$OK"
pod '' > "$tmp/pods.json"
LOG_MODE=fail run "S2 status: empty value" 1 "$NOREAD"
pod "$REF" > "$tmp/pods.json"
GETPODS_FAIL_AT=3 LOG_MODE=fail run "S3 status: fails once, then recovers" 0 "$OK"
GETPODS_FAIL_FROM=3 LOG_MODE=fail run "S4 status: fails for good" 1 "$NOREAD"

# ── kubectl logs (the Pod status carries no message) ─────────────────────────
pod '' > "$tmp/pods.json"
LOG_MODE=ok LOG_TEXT='error="catalog-rpc: --require-durable: postgres.url is empty"' run "L1 logs: success" 0 "$OK"
LOG_MODE=empty run "L2 logs: empty value" 1 "$NOREAD"
LOG_MODE=flaky LOG_TEXT='error="catalog-rpc: --require-durable: postgres.url is empty"' run "L3 logs: fail, then recover" 0 "$OK"
LOG_MODE=fail run "L4 logs: fail for good" 1 "$NOREAD"

# ── the two ways a reader lies ───────────────────────────────────────────────
LOG_MODE=tool-error run "T the tool's error text is not the app's log" 1 "$NOREAD"
LOG_MODE=ok LOG_TEXT="error=\"--require-durable: postgres.url is empty key=$key\"" run "K a refusal carrying the key" 1 "carries the service key"

# ── the rpc-pod phase ────────────────────────────────────────────────────────
pod "$REF" > "$tmp/pods.json"
# R1 the Secret without APP_CATALOG_API_KEY stops the rpc pod: covered by every case above (RPC_PODS=broken is the default)
# R2 it did not: the script must fail, bounded, and still clean up
RPC_PODS=healthy LOG_MODE=fail run "R2 rpc pod: the missing key did not stop it" 1 "did not stop the rpc pod"
# R3 a values file without secrets.postgresUrl: no fixture to build the Secret from → fails before touching the cluster further, still cleans up
printf 'secrets: {}\n' > "$tmp/novalues.yaml"
VALUES_FILE="$tmp/novalues.yaml" LOG_MODE=fail run "R3 values without secrets.postgresUrl" 1 "no secrets.postgresUrl"
