#!/usr/bin/env bash
# Upgrading from the chart that runs `payment-rpc consume` to the chart that runs payment-worker (#410 slice 2):
#   payment-upgrade.sh <namespace> <release> <chart-dir> <values-file> <flow-values-file>
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
# The release is restored to <values-file> on the way out, whatever happens. Needs kubectl, helm, curl, jq, git, tar.
set -euo pipefail
ns=$1 release=$2 chart=$3 values=$4 flow=$5
from_tag=${PAYMENT_UPGRADE_FROM_TAG:-v0.63.3}
from_image=${PAYMENT_UPGRADE_FROM_IMAGE:-0.63.3}
rport=${UPGRADE_RPC_PORT:-18096} sport=${UPGRADE_SERVE_PORT:-18097}
api="http://127.0.0.1:$rport/api" serve="http://127.0.0.1:$sport"
pfs=""
body=""
tmp=$(mktemp -d)
dbname="" # the database this run creates for itself (stage 1); the release's own database is never modified
# adb talks to the release's own database ("app") and is used for exactly two statements: CREATE DATABASE and
# DROP DATABASE of $dbname. Everything else goes through db(), on $dbname.
adb() { kubectl -n "$ns" exec deploy/postgres -- psql -U app -d app -tAq -c "$1"; }

fail() {
  echo "payment-upgrade: $*" >&2
  [ -z "${soft:-}" ] || exit 1
  kubectl -n "$ns" get pods,jobs >&2 2>/dev/null || true
  # The migrate hook Job is what fails an upgrade onto a schema its image does not know: say why, not just that it did.
  echo "payment-upgrade: logs of the $release-migrate Job (hook pods are kept after a failure):" >&2
  kubectl -n "$ns" logs -l "job-name=$release-migrate" --tail=50 --prefix --all-containers >&2 2>&1 || true
  exit 1
}
restore() {
  local rc=$?
  # The background helm of stage 4 may still be running (an order failed to be placed while it ran). Let it
  # finish FIRST: a restore that ran before it would be overwritten by it, and the release would end upgraded.
  if [ -n "${helm_pid:-}" ]; then wait "$helm_pid" 2>/dev/null || true; fi
  for p in $pfs; do kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; done
  rm -rf "$tmp"
  helm upgrade --install "$release" "$chart" -n "$ns" -f "$values" --wait --timeout 3m >/dev/null 2>&1 || true
  # The release is back on its own database. The one database this script made is the only thing it ever drops;
  # FORCE ends the connections of pods that were still on it. Nothing in `app` (or any other database) is touched.
  if [ -n "${dbname:-}" ] && [ "$dbname" != app ]; then
    adb "DROP DATABASE IF EXISTS \"$dbname\" WITH (FORCE)" >/dev/null 2>&1 || echo "payment-upgrade: could not drop the database $dbname it created" >&2
  fi
  return "$rc"
}
trap restore EXIT

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
db() { kubectl -n "$ns" exec deploy/postgres -- psql -U app -d "$dbname" -tAq -c "$1"; }

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
# the premise: that chart really runs `payment-rpc consume`, and this one does not
old_render=$(helm template t "$old_chart" -f "$values" -f "$flow" "${old_args[@]}")
new_render=$(helm template t "$chart" -f "$values" -f "$flow")
grep -q -- '"consume"' <<<"$old_render" || fail "the chart at $from_tag does not run 'payment-rpc consume': nothing to upgrade from"
if grep -q -- '"consume"' <<<"$new_render"; then fail "this chart still runs a 'consume' argument"; fi
old_up() { helm upgrade --install "$release" "$old_chart" -n "$ns" -f "$values" -f "$flow" "${old_args[@]}" "$@" --wait --timeout 6m >/dev/null || fail "the previous chart did not become ready"; forwards; }
# The release's own database already holds the schema the TREE's migrate image built (earlier stages of helm-kind installed
# this tree), so it can be ahead of what the previous release's migrate image knows. Observed (v0.63.3 against ordering
# at 10, #427):
#   migrate ordering: no migration found for version 10: read down for version 10 .: file does not exist
# and the release's pre-upgrade hook Job fails. Stage 1 is the previous release coming up from nothing, so it gets a
# database of its own: created here on the same Postgres, named for this run, and handed to the chart through
# secrets.postgresUrl (the one input every Secret of the chart is built from). The old release's migrate hook builds in it
# exactly what that release knows; stage 4 upgrades it to this tree, which is the upgrade path this script is about. No
# migration number appears here. The release's own database is not read or changed, and `kind` clusters are reused
# between runs, so it may hold things that are not this test's: the only DROP this script runs is of $dbname, in restore().
grep -qxF '  postgresUrl: postgres://app:app@postgres:5432/app?sslmode=disable' "$values" || fail "$values does not give secrets.postgresUrl as postgres://app:app@postgres:5432/app?sslmode=disable: cannot derive the run's database URL from it"
dbname="upgrade_$(date +%s)_$RANDOM"
pgurl="postgres://app:app@postgres:5432/$dbname?sslmode=disable"
adb "CREATE DATABASE \"$dbname\"" >/dev/null || { dbname=""; fail "could not create the database for this run"; }
old_args+=(--set "secrets.postgresUrl=$pgurl")
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
helm upgrade --install "$release" "$chart" -n "$ns" -f "$values" -f "$flow" --set "secrets.postgresUrl=$pgurl" --wait --timeout 6m >/dev/null &
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
