#!/usr/bin/env bash
# Upgrading from the chart that runs `payment-rpc consume` to the chart that runs payment-worker (#410 slice 2):
#   payment-upgrade.sh <home-namespace> <home-release> <chart-dir> <values-file> <flow-values-file>
#
# The drill runs in a namespace of its own (x-monorepo-upgrade-<run>), with its own throwaway Postgres and Redis (deps.yaml)
# and its own release (x-monorepo-upg-<rand>): it never reads or writes anything of <home-namespace> / <home-release>, which
# `task helm-kind` reuses between runs and whose Postgres may hold things that are not this test's (#427, #429). Both
# arguments are only there for the caller (payment-upgrade-guard.sh checks that the home database is unchanged). The
# namespace is created with a label of this run and, on the way out (whatever happens), it is the one namespace deleted.
# This chart renders no cluster-scoped object (the check below refuses a render that would) and the webhook Ingress is
# switched off, so two releases cannot collide.
#
# The consumer's cursor is its consumer group (payment-rpc.payment), so what has to hold across the switch is:
# the same group (no new cursor: a new one would start from worker.start_from and replay or skip), a cursor that
# never moves back, and every OrderConfirmed handled exactly once, whether it was published before the switch,
# while no consumer was running, while the Deployment was rolling, or after.
#
#   1  install the PREVIOUS chart (git tag $PAYMENT_UPGRADE_FROM_TAG, default v0.63.3) with its own published images
#      (ghcr.io, tag $PAYMENT_UPGRADE_FROM_IMAGE, default 0.63.3): its payment-consume Deployment runs
#      `payment-rpc consume`. Proven by the rendered chart before it is installed.
#   2  order A is placed: the OLD consumer creates its payment. The cursors and the inbox are recorded.
#   3  the OLD consumer is scaled to zero (a helm value, so the next upgrade is not fooled by a live replica count)
#      and order B is placed: it is CONFIRMED, no payment exists, and none appears while nothing consumes.
#   4  the release is upgraded to THIS chart (images built from the tree, payment-worker) while orders D1..D3 are
#      placed one after another (the Deployment rolls under them).
#   5  order C is placed after the switch.
# Then, on the database: the set of consumer groups on every topic payment reads is the one from step 2; each cursor
# is at least where it was and has caught up with the end of its topic; every order has exactly one payment; the
# consumer inbox grew by exactly one row per OrderConfirmed published since step 2 (nothing replayed, nothing
# skipped). The consumer runs the payment-worker image under the container name payment-worker. An order paid
# before the switch (A, whose checkout was started by the old pods) and one paid after (C) both reach COMPLETED.
# Needs kubectl, helm, curl, jq, git, tar, python3 with PyYAML.
set -euo pipefail
# refuses to run outside run.sh (#444): HOME is the runner's scratch directory, which holds the marker
[ "${KIND_RUNNER_HOME:-}" = "${HOME:-}" ] && [ -f "${HOME:-/nonexistent}/.kind-runner" ] || { echo "$0: run it through its task (task helm-kind-up / helm-kind / helm-kind-keda; CLUSTER=<name> picks the cluster): the task isolates it from ~/.kube/config (#444)" >&2; exit 2; }
# Internal state is never taken from the environment: cleanup() decides what to delete from ns_uid, so a parent
# that exported them (or a stale export in a shell) must not be able to make it delete a namespace this run did not create.
# They start empty here, before the trap exists. The environment variables this script reads are, all of them:
#   UPGRADE_RUN_ID (digits-digits; names the namespace, proves nothing about ownership), PAYMENT_UPGRADE_FROM_TAG,
#   PAYMENT_UPGRADE_FROM_IMAGE, UPGRADE_RPC_PORT, UPGRADE_SERVE_PORT,
# plus what the tools themselves read (PATH, KUBECONFIG, HOME, git / helm / kubectl settings).
ns_uid="" token="" helm_pid="" soft="" pfs="" body="" res="" maybe_ns=""
# Every input is checked here, before the trap exists and before anything is created, sent or installed. The inputs are,
# all of them (#432): the five arguments ($1 and $2, the home namespace and release, are deliberately not used; $3-$5 are
# paths handed to helm and python), and the variables listed above. Each one ends up in a URL (the two ports: curl sends the
# X-API-Key to it), in a helm argument (--set image.tag=...), in a git ref (the tag), or in a name (the run id); each is
# matched whole against the one shape it may have, in the C locale, and anything else is refused, not repaired.
refuse() { echo "payment-upgrade: refused: $*" >&2; exit 1; }
export LC_ALL=C
[ "$#" -eq 5 ] || refuse "want 5 arguments (home namespace, home release, chart dir, values file, flow values file), got $#"
chart=$3 values=$4 flow=$5
[[ $chart != -* && $values != -* && $flow != -* ]] || refuse "a chart / values path must not start with '-'"
[ -d "$chart" ] || refuse "the chart directory $chart does not exist"
[ -f "$values" ] && [ -f "$flow" ] || refuse "the values file $values or the flow values file $flow does not exist"
kind_dir=$(cd "$(dirname "$0")" && pwd)
run_id=${UPGRADE_RUN_ID-$(date +%s)-$RANDOM} # set-but-empty is refused below, not replaced by a default
[[ $run_id =~ ^[0-9]{1,12}-[0-9]{1,6}$ ]] || refuse "UPGRADE_RUN_ID must be <digits>-<digits>"
from_tag=${PAYMENT_UPGRADE_FROM_TAG-v0.63.3}
[[ $from_tag =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] || refuse "PAYMENT_UPGRADE_FROM_TAG must be a tag name ([A-Za-z0-9._-], not starting with . or -)"
from_image=${PAYMENT_UPGRADE_FROM_IMAGE-0.63.3}
[[ $from_image =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || refuse "PAYMENT_UPGRADE_FROM_IMAGE must be an image tag ([A-Za-z0-9_.-], at most 128), it is put in a helm --set"
# A port is a decimal integer 1-65535 with no sign, no leading zero, nothing else: it is put in http://127.0.0.1:<port>, and
# a value like 1@host:80 would make that URL name another host, which would then get the X-API-Key.
port_ok() { [[ $1 =~ ^[1-9][0-9]{0,4}$ ]] && [ "$1" -le 65535 ]; }
rport=${UPGRADE_RPC_PORT-18096} sport=${UPGRADE_SERVE_PORT-18097}
port_ok "$rport" || refuse "UPGRADE_RPC_PORT must be a decimal integer 1-65535 (no sign, no leading zero, nothing else)"
port_ok "$sport" || refuse "UPGRADE_SERVE_PORT must be a decimal integer 1-65535 (no sign, no leading zero, nothing else)"
[ "$rport" != "$sport" ] || refuse "UPGRADE_RPC_PORT and UPGRADE_SERVE_PORT must differ"
ns="x-monorepo-upgrade-$run_id"
release="x-monorepo-upg-${run_id##*-}" # contains the chart name, so the Jobs are <release>-migrate, as in any release named x-monorepo*
run_label=x-monorepo-upgrade-drill
api="http://127.0.0.1:$rport/api" serve="http://127.0.0.1:$sport"
tmp=$(mktemp -d)
url_check=$kind_dir/postgres-url-check.py

fail() {
  echo "payment-upgrade: $*" >&2
  [ -z "${soft:-}" ] || exit 1
  if [ -n "$ns_uid" ]; then
    kubectl -n "$ns" get pods,jobs >&2 2>/dev/null || true
    # The migrate hook Job is what fails an upgrade onto a schema its image does not know: say why, not just that it did.
    echo "payment-upgrade: logs of the $release-migrate Job (hook pods are kept after a failure):" >&2
    kubectl -n "$ns" logs -l "job-name=$release-migrate" --tail=50 --prefix --all-containers >&2 2>&1 || true
  fi
  exit 1
}
cleanup() {
  local rc=$?
  # The background helm of stage 4 may still be running (an order failed to be placed while it ran). Let it finish
  # FIRST: deleting the namespace under it would leave it failing against nothing.
  if [ -n "${helm_pid:-}" ]; then wait "$helm_pid" 2>/dev/null || true; fi
  for p in $pfs; do kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; done
  rm -rf "$tmp"
  # The one destructive step, bound to the identity the API server gave this run's namespace WHEN IT CREATED IT (ns_uid, taken
  # from the create's own answer and never looked up again: a lookup at cleanup time would find whatever carries the token
  # then, and a namespace deleted and re-made under the same name and label would have its new uid accepted). The delete has
  # that uid as a precondition, so a namespace that is not the one this run created is refused (409) and nothing is touched.
  # No uid (nothing was created, or the create's answer was lost) means nothing is deleted. There is no `helm uninstall`:
  # the release's storage (helm's Secrets) and everything it installed live in the namespace and go with it.
  # The create was sent but its uid was never kept (killed between the two, or before the answer came): the namespace may
  # exist and nothing here may delete it, since only the uid proves it is this run's. Say what to look for (#433).
  if [ -n "$maybe_ns" ] && [ -z "$ns_uid" ]; then
    echo "payment-upgrade: interrupted before the uid of namespace $ns was kept: if it exists (label $run_label=$token) it is left in place, delete it by hand" >&2
  fi
  if [ -n "$ns_uid" ]; then
    printf '{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":"%s"}}' "$ns_uid" |
      kubectl delete --raw "/api/v1/namespaces/$ns" -f - >/dev/null 2>&1 || echo "payment-upgrade: left the namespace $ns alone: it is gone, or no longer the one this run created" >&2
  fi
  return "$rc"
}
trap cleanup EXIT

forwards() {
  for p in $pfs; do kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; done
  pfs=""
  kubectl -n "$ns" port-forward "svc/$release-rpc" "$rport:80" >/dev/null 2>&1 & pfs="$pfs $!"
  kubectl -n "$ns" port-forward "svc/$release-payment-serve" "$sport:80" >/dev/null 2>&1 & pfs="$pfs $!"
  for _ in $(seq 1 40); do curl -fsS "$api/readyz" >/dev/null 2>&1 && curl -fsS "$serve/api/readyz" >/dev/null 2>&1 && return 0; sleep 1; done
  fail "rpc / payment-rpc serve never answered readyz through the port-forward"
}

forwards_soft() { # re-point the port-forwards at whatever pods are there now; never fatal (used while pods roll)
  for p in $pfs; do kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; done
  pfs=""
  kubectl -n "$ns" port-forward "svc/$release-rpc" "$rport:80" >/dev/null 2>&1 & pfs="$pfs $!"
  kubectl -n "$ns" port-forward "svc/$release-payment-serve" "$sport:80" >/dev/null 2>&1 & pfs="$pfs $!"
  for _ in $(seq 1 15); do curl -fsS "$api/readyz" >/dev/null 2>&1 && return 0; sleep 1; done
  return 1
}

shopper_key=$(sed -n 's/.*shopper=\([A-Za-z0-9._-]*\).*/\1/p' "$flow" | head -1)
admin_key=$(sed -n 's/.*admin=\([A-Za-z0-9._-]*\):admin.*/\1/p' "$flow" | head -1)
[ -n "$shopper_key" ] && [ -n "$admin_key" ] || fail "no shopper / admin key in $flow"

call() {
  local out
  out=$(curl -sS -D - -o "$tmp/body" -H 'Content-Type: application/json' -H "X-API-Key: $1" "$api/$2" -d "$3" || true)
  body=$(cat "$tmp/body" 2>/dev/null || true)
  res="$(sed -n '1s/^HTTP[^ ]* \([0-9]*\).*/\1/p' <<<"$out") $(tr -d '\r' <<<"$out" | sed -n 's/^[Xx]-[Ee]rror-[Cc]ode: //p' | head -n 1 | grep . || echo -)"
}
want() { [ "$2" = "$3" ] || fail "$1: got '$2', want '$3' (body: ${body:-})"; }
as_shopper() { call "$shopper_key" "$@"; }
as_admin() { call "$admin_key" "$@"; }
db() { kubectl -n "$ns" exec deploy/postgres -- psql -U app -d app -tAq -c "$1"; }

order_status() { as_shopper ordering.v1.OrderService/GetOrder "{\"id\":\"$1\"}" >/dev/null; jq -r '.order.status // "-"' <<<"$body"; }
wait_order() { # <order> <ORDER_STATUS_X> [seconds]
  local s=""
  for _ in $(seq 1 "${3:-60}"); do s=$(order_status "$1"); [ "$s" = "$2" ] && return 0; sleep 1; done
  fail "order $1 is '$s' after ${3:-60}s, want $2"
}
pay_status() { as_shopper payment.v1.PublicPaymentService/GetPayment "{\"order_id\":\"$1\"}" >/dev/null; jq -r '.payment.status // "-"' <<<"$body"; }
wait_payment() { # <order> <PAYMENT_STATUS_X> [seconds]
  local s=""
  for _ in $(seq 1 "${3:-90}"); do s=$(pay_status "$1"); [ "$s" = "$2" ] && return 0; sleep 1; done
  fail "payment of order $1 is '$s' after ${3:-90}s, want $2"
}
place() { # -> the id of an order that is CONFIRMED under the switch (stock seeded by the admin, ordered by the shopper)
  local sku; sku="upg-$(date +%s)-$RANDOM"
  as_admin catalog.v1.CatalogService/UpsertProduct "{\"sku\":\"$sku\",\"name\":\"$sku\",\"price\":{\"amount_minor\":100,\"currency\":\"USD\"}}"; want "UpsertProduct" "${res%% *}" 200
  as_admin inventory.v1.InventoryService/SetStock "{\"sku\":\"$sku\",\"on_hand\":10}"; want "SetStock" "${res%% *}" 200
  as_shopper ordering.v1.OrderService/CreateOrder "{\"customer_id\":\"shopper\",\"items\":[{\"sku\":\"$sku\",\"quantity\":1}]}"; want "CreateOrder" "${res%% *}" 200
  local id; id=$(jq -r .order.id <<<"$body")
  wait_order "$id" ORDER_STATUS_CONFIRMED
  printf '%s' "$id"
}
start_payment() { as_shopper payment.v1.PublicPaymentService/StartPayment "{\"order_id\":\"$1\"}"; want "StartPayment($1)" "${res%% *}" 200; }
mock() { kubectl -n "$ns" exec "deploy/$release-payment-serve" -- /app mock "$@"; }
hook="http://127.0.0.1:8080/api/payment/webhook" # the webhook route, from inside the serve pod

# The topics payment reads (ordering's three order events, payment's own two work items) and their cursors.
topics="ordering.events_v1_orderconfirmed ordering.events_v1_ordercancelled ordering.events_v1_ordercompleted payment.events_v1_paymentrefundrequested payment.events_v1_webhookreceived"
cursors() { # every "<table> <group> <acked>" row, sorted: the full state of the consumer's position
  local t
  for t in $topics; do db "SELECT '$t', consumer_group, offset_acked FROM ${t}_offsets" | tr '|' ' '; done | sort
}
groups_of() { awk '{print $1" "$2}' <<<"$1" | sort -u; }
inbox_rows() { db "SELECT count(*) FROM payment.inbox"; }

# ── 1 the previous chart, as it was released ───────────────────────────────────────────────────────────────
# --depth only when this clone is already shallow (CI): on a repository shared by worktrees it would shallow all of them
depth=(); [ "$(git rev-parse --is-shallow-repository)" != true ] || depth=(--depth=1)
git fetch -q ${depth[@]+"${depth[@]}"} origin "refs/tags/$from_tag:refs/tags/$from_tag" 2>/dev/null || true
git rev-parse -q --verify "refs/tags/$from_tag" >/dev/null || fail "the tag $from_tag is not available: the previous chart cannot be installed"
mkdir -p "$tmp/old"
git archive "$from_tag" deployments/helm/x-monorepo | tar -x -C "$tmp/old"
old_chart="$tmp/old/deployments/helm/x-monorepo"
old_args=(--set "image.registry=ghcr.io/blackhorseya/x-monorepo" --set "image.tag=$from_image" --set "image.pullPolicy=IfNotPresent")
# The webhook Ingress is switched off for both charts: another release in another namespace already has the same host and
# path, and the ingress controller refuses duplicates. The drill reaches the pods through port-forwards.
common_args=(--set paymentRpc.ingress.enabled=false)
# One render per chart, with exactly the arguments of its installs below: the premise (that chart really runs `payment-rpc
# consume`, and this one does not) and the connection check both read it.
old_render=$(helm template t "$old_chart" -f "$values" -f "$flow" "${old_args[@]}" "${common_args[@]}") || fail "refused: the previous chart does not render with these values"
new_render=$(helm template t "$chart" -f "$values" -f "$flow" "${common_args[@]}") || fail "refused: this chart does not render with these values"
grep -q -- '"consume"' <<<"$old_render" || fail "the chart at $from_tag does not run 'payment-rpc consume': nothing to upgrade from"
if grep -q -- '"consume"' <<<"$new_render"; then fail "this chart still runs a 'consume' argument"; fi
old_up() { helm upgrade --install "$release" "$old_chart" -n "$ns" -f "$values" -f "$flow" "${old_args[@]}" "${common_args[@]}" "$@" --wait --timeout 6m >/dev/null || fail "the previous chart did not become ready"; forwards; }
# Why a namespace of its own: the home database already holds the schema the TREE's migrate image built, so it can be ahead
# of what the previous release's migrate image knows. Observed (v0.63.3 against ordering at 10, #427):
#   migrate ordering: no migration found for version 10: read down for version 10 .: file does not exist
# and the release's pre-upgrade hook Job fails. Stage 1 is the previous release coming up from nothing, so it gets a Postgres
# (and Redis) of its own, from deps.yaml, in a namespace of its own. The old release's migrate hook builds in it exactly
# what that release knows, stage 4 upgrades it to this tree (the path this script is about), and no migration number
# appears anywhere. The URLs in <values-file> name the Services by their short names (postgres, redis), which resolve to the
# ones in the drill's namespace.
#
# What decides where a workload connects is not a list this script can keep up with (secrets.postgresUrl, every
# existingSecret, every extraEnv, postgres.url / redis.url in config, whatever the next chart version adds), so the RENDERED
# charts are checked with exactly the arguments of the installs below: every container of every workload and hook must resolve
# APP_POSTGRES_URL and APP_REDIS_URL to the values of <values-file> (env, envFrom and secretKeyRef followed through the
# Secrets of the same render), and the render must hold no Ingress, no cluster-scoped object and no object with a namespace of
# its own. An existingSecret, a fully qualified name that points at another namespace, or anything else it cannot resolve is
# refused. Fail closed, before anything is created or installed.
read -r pgurl redisurl < <(python3 -c 'import sys,yaml; s=yaml.safe_load(open(sys.argv[1]))["secrets"]; print(s["postgresUrl"], s["redisUrl"])' "$values") || fail "refused: $values gives no secrets.postgresUrl / secrets.redisUrl"
[[ $pgurl =~ ^postgres://[^@/]*@postgres:[0-9]+/ && $redisurl =~ ^redis://redis:[0-9]+/ ]] || fail "refused: $values must name Postgres and Redis by their short Service names (postgres, redis), which resolve inside the drill's namespace"
assert_connections() { # <rendered chart>
  python3 "$url_check" --expect "APP_POSTGRES_URL=$pgurl" --expect "APP_REDIS_URL=$redisurl" <<<"$1" >&2 || fail "refused: with these values a workload would not connect only to the drill's own Postgres and Redis"
}
assert_connections "$old_render"
assert_connections "$new_render"
# Only now is anything created. A namespace that already exists is never ours, whatever it is labelled: run_id can be given by
# the caller, so a label made of it proves nothing. The token (random, generated here) is only a marker for a human; ownership is
# the uid in the create's own answer. If that answer is lost the namespace is NOT adopted: the run stops and says what to delete.
if kubectl get namespace "$ns" -o name >/dev/null 2>&1; then fail "refused: the namespace $ns already exists: it is not reused and not touched"; fi
token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
maybe_ns=1 # from here on the namespace may exist without this run knowing its uid (cleanup reports that, see above)
if ! created=$(kubectl create -f - -o jsonpath='{.metadata.uid}' 2>"$tmp/create.err" <<NSEOF
apiVersion: v1
kind: Namespace
metadata:
  name: $ns
  labels:
    $run_label: "$token"
NSEOF
); then
  if grep -q AlreadyExists "$tmp/create.err"; then maybe_ns=""; fail "refused: the namespace $ns already exists: it is not reused and not touched"; fi
  if [ "$(kubectl get namespace -l "$run_label=$token" -o name 2>/dev/null)" = "namespace/$ns" ]; then
    maybe_ns=""
    fail "refused: the create of $ns was applied but its answer was lost, so its uid is unknown and nothing will be deleted: namespace $ns (label $run_label=$token) is left in place, delete it by hand"
  fi
  maybe_ns=""
  fail "refused: could not create the namespace $ns: $(head -c 300 "$tmp/create.err")"
fi
[[ $created =~ ^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$ ]] || { maybe_ns=""; fail "refused: the create of $ns did not answer with a namespace uid (got '$created'), so nothing will be deleted: if namespace $ns (label $run_label=$token) exists, delete it by hand"; }
ns_uid=$created
kubectl -n "$ns" apply -f "$kind_dir/deps.yaml" >/dev/null
kubectl -n "$ns" rollout status deploy/postgres deploy/redis --timeout=2m >/dev/null || fail "the drill's Postgres / Redis did not become ready"
old_up
oldimg=$(kubectl -n "$ns" get deploy "$release-payment-consume" -o jsonpath='{.spec.template.spec.containers[0].image}')
case $oldimg in */payment-rpc:*) ;; *) fail "before the upgrade the consumer runs '$oldimg', want the payment-rpc image" ;; esac
echo "payment-upgrade: the previous chart ($from_tag) is up; its consumer runs $oldimg consume"

# ── 2 order A under the old consumer ───────────────────────────────────────────────────────────────────────
A=$(place); wait_payment "$A" PAYMENT_STATUS_PENDING; start_payment "$A"
sleep 3
before=$(cursors); inbox_before=$(inbox_rows)
conf_end_before=$(db "SELECT coalesce(max(\"offset\"), 0) FROM ordering.events_v1_orderconfirmed"); payments_before=$(db "SELECT count(*) FROM payment.payments")
[ -n "$before" ] || fail "no cursor rows before the upgrade: the premise (a consumer with a cursor) does not hold"
grep -q ' payment-rpc.payment ' <<<"$before" || fail "the old consumer's cursor is not payment-rpc.payment: $before"
echo "payment-upgrade: order $A has its payment (old consumer); cursors before:"; sed 's/^/    /' <<<"$before"

# ── 3 no consumer: B is confirmed and nothing turns it into a payment ──────────────────────────────────────
old_up --set paymentRpc.consume.replicaCount=0
kubectl -n "$ns" wait --for=delete pod -l "app.kubernetes.io/component=payment-consume" --timeout=2m >/dev/null 2>&1 || true
B=$(place)
for _ in $(seq 1 6); do
  [ "$(pay_status "$B")" = "-" ] || fail "order $B has a payment while no consumer runs: something else consumes OrderConfirmed"
  sleep 1
done
echo "payment-upgrade: order $B is CONFIRMED and has no payment while the consumer is down"

# ── 4 the upgrade, with orders arriving while it rolls ─────────────────────────────────────────────────────
# helm runs in the background; the orders are placed through port-forwards that are re-pointed before every try (the
# rpc pods roll too, and a forward dies with its pod). A try that fails is repeated; an order that was created but
# whose answer was lost is still counted, because the checks below count the events on the database, not the orders
# this script remembers.
helm upgrade --install "$release" "$chart" -n "$ns" -f "$values" -f "$flow" "${common_args[@]}" --wait --timeout 6m >/dev/null &
helm_pid=$!
during=""
for i in 1 2 3; do
  id=""
  for _ in $(seq 1 8); do
    forwards_soft || { sleep 2; continue; }
    if id=$(soft=1 place 2>/dev/null) && [ -n "$id" ]; then break; fi
    id=""; sleep 2
  done
  [ -n "$id" ] || fail "order D$i could not be placed while the Deployments rolled"
  during="$during $id"
  sleep 2
done
wait "$helm_pid" || fail "the upgrade to this chart did not become ready"
forwards

# ── 5 order C after the switch ─────────────────────────────────────────────────────────────────────────────
C=$(place)

orders="$A $B $C$during"
for o in $orders; do wait_payment "$o" PAYMENT_STATUS_PENDING 120; done
sleep 3
after=$(cursors); inbox_after=$(inbox_rows)
conf_end_after=$(db "SELECT coalesce(max(\"offset\"), 0) FROM ordering.events_v1_orderconfirmed"); payments_after=$(db "SELECT count(*) FROM payment.payments")

# the consumer is the payment-worker image under the container name payment-worker, and it started
newimg=$(kubectl -n "$ns" get deploy "$release-payment-consume" -o jsonpath='{.spec.template.spec.containers[0].image}')
newname=$(kubectl -n "$ns" get deploy "$release-payment-consume" -o jsonpath='{.spec.template.spec.containers[0].name}')
case $newimg in */payment-worker:*) ;; *) fail "after the upgrade the consumer runs '$newimg', want the payment-worker image" ;; esac
[ "$newname" = payment-worker ] || fail "the consumer container is named '$newname', want payment-worker"
clog=$(kubectl -n "$ns" logs "deploy/$release-payment-consume" 2>/dev/null || true)  # not piped into grep -q: with pipefail an early grep exit makes kubectl fail
grep -q 'payment-worker started' <<<"$clog" || fail "the new consumer never logged that it started"

# same groups, no new cursor
[ "$(groups_of "$before")" = "$(groups_of "$after")" ] || fail "the consumer groups changed across the upgrade. before: $(groups_of "$before" | tr '\n' ';') after: $(groups_of "$after" | tr '\n' ';')"
if grep -qE ' (payment-worker[^ ]*|worker\.payment) ' <<<"$after"; then fail "a cursor under a new group appeared: $after"; fi
# no cursor moved back, and every one is at the end of its topic
while read -r t g acked; do
  prev=$(awk -v t="$t" -v g="$g" '$1==t && $2==g {print $3}' <<<"$before")
  [ "$acked" -ge "$prev" ] || fail "the cursor of $g on $t moved back: $prev -> $acked"
  [ "$g" = payment-rpc.payment ] || continue
  end=$(db "SELECT coalesce(max(\"offset\"), 0) FROM $t")
  [ "$acked" -ge "$end" ] || fail "the cursor of $g on $t is at $acked, the topic ends at $end: events were skipped or are still unread"
done <<<"$after"

# exactly one payment per order, and one inbox row per event published since
for o in $orders; do
  n=$(db "SELECT count(*) FROM payment.payments WHERE order_id = '$o'")
  [ "$n" = 1 ] || fail "order $o has $n payments, want exactly 1"
done
new_events=$((conf_end_after - conf_end_before))
[ "$new_events" -ge "$(wc -w <<<"$B $C$during")" ] || fail "only $new_events OrderConfirmed events were published since the snapshot, fewer than the orders placed"
delta=$((inbox_after - inbox_before))
[ "$delta" = "$new_events" ] || fail "the consumer inbox grew by $delta rows, want $new_events (one per OrderConfirmed published since the snapshot: nothing replayed, nothing skipped)"
pdelta=$((payments_after - payments_before))
[ "$pdelta" = "$new_events" ] || fail "$pdelta payments were created for $new_events new OrderConfirmed events: want one each"

# an order the old pods started, and one started after, both complete through the new consumer
mock pay --order "$A" --deliver "$hook" >/dev/null || fail "mock pay (A)"
start_payment "$C"
mock pay --order "$C" --deliver "$hook" >/dev/null || fail "mock pay (C)"
wait_order "$A" ORDER_STATUS_COMPLETED 90
wait_order "$C" ORDER_STATUS_COMPLETED 90

echo "payment-upgrade: cursors after:"; sed 's/^/    /' <<<"$after"
echo "payment-upgrade ok: $from_tag -> this chart: consumer group unchanged (payment-rpc.payment), cursors never moved back and are at the end, $(wc -w <<<"$orders") orders have exactly one payment each, inbox +$delta and payments +$pdelta = $new_events new OrderConfirmed events; A (paid after the switch, started before) and C (after) are COMPLETED; the consumer runs $newimg"
