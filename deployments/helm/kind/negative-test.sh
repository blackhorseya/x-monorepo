#!/usr/bin/env bash
# negative.sh's reading of the refusal, with fake kubectl / helm / jq-free pods: no cluster needed.
# Proves the assertions can go red (fault injection) and that a failed read is retried, not asserted on:
#   1. Pod status has the refusal                     → ok
#   2. Pod status empty, logs fail twice then answer  → ok (retried; the tool's error text is never "the log")
#   3. Pod status empty, logs always fail             → fails, naming that no refusal was readable
#   4. Refusal present but it carries the service key → fails
#   5. Failure text is the tool's error, not a refusal → fails (does not grep the error as app output)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
key="kind-neg-key-0123456789abcd"

# fake helm: succeed. fake kubectl: serve the pod JSON in $POD_JSON, logs per $LOG_MODE.
cat > "$tmp/bin/helm" <<'H'
#!/usr/bin/env bash
exit 0
H
cat > "$tmp/bin/kubectl" <<'K'
#!/usr/bin/env bash
args="$*"
case $args in
  *"create secret"*|*"delete secret"*) exit 0 ;;
  *"get pods"*)
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
  out=$(PATH="$tmp/bin:$PATH" PHASE_FILE="$tmp/phase" tmp_count="$tmp/count" POD_JSON="$tmp/pods.json" NEGATIVE_TRIES=4 NEGATIVE_SLEEP=0 \
    "$here/negative.sh" ns rel chart values 2>&1) && got=0 || got=1
  rm -f "$tmp/phase" "$tmp/count"
  [ "$got" = "$2" ] || { echo "negative-test: $1: exit $got, want $2"; echo "$out"; exit 1; }
  case $out in *"$3"*) ;; *) echo "negative-test: $1: output lacks '$3'"; echo "$out"; exit 1 ;; esac
  echo "negative-test ok: $1"
}

pod 'level=ERROR msg="catalog-rpc exited" error="catalog-rpc: --require-durable: postgres.url is empty"' > "$tmp/pods.json"
LOG_MODE=fail run "refusal in the Pod status" 0 "negative ok: an empty APP_POSTGRES_URL"

pod '' > "$tmp/pods.json"
LOG_MODE=flaky LOG_TEXT='error="catalog-rpc: --require-durable: postgres.url is empty"' run "empty status, flaky logs are retried" 0 "negative ok: an empty APP_POSTGRES_URL"
LOG_MODE=fail run "empty status, logs always fail" 1 "no readable --require-durable refusal"
LOG_MODE=tool-error run "the tool's error text is not the app's log" 1 "no readable --require-durable refusal"
LOG_MODE=ok LOG_TEXT="error=\"--require-durable: postgres.url is empty key=$key\"" run "a refusal carrying the key" 1 "carries the service key"
