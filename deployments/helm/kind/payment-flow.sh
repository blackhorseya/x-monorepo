#!/usr/bin/env bash
# The whole payment flow through the installed release, ordering.require_payment ON (docs/payment.md §5, §8, §9 S6):
#   payment-flow.sh <namespace> <release> <chart-dir> <values-file> <flow-values-file>
# The release is upgraded to <values-file> + <flow-values-file> first (the switch on, auth required with two static
# keys, payment-rpc's mock control on) and restored to <values-file> on the way out, whatever happens.
#
#   1  create -> reserved -> CONFIRMED (snapshot true) -> StartPayment -> pay -> webhook -> COMPLETED
#   2  delayed webhook: paid at the provider, the order waits; then the webhook arrives. And the webhook never arrives:
#      the expire sweep finds the money (T6)
#   3  redelivery: the same notification again and again changes nothing
#   4  out of order / foreign: an unknown event type, a notification for a checkout nobody owns
#   5  the customer cancels while paying; the money arrives late: refunded, the order stays CANCELLED
#   6  the window runs out; the money arrives late: the order was cancelled, refunded
#   7  the whole TTL sequence with the provider down (reservation TTL -> OrderCancelled -> T7 -> failed sweeps ->
#      recovery -> T8 -> refund), driven by the real CronJobs
#   8  the switch: on -> off (an order that was CONFIRMED under it is not touched), off -> on (an order from before
#      it is not touched)
#   9  payment-rpc stopped: ordering and the reservation go on; payment operations answer 503; the same rpc pods work
#      again when it is back
# Needs kubectl + helm + curl + jq. Exits non-zero (with the pods' state) when anything is off.
# Unless it runs to its end, diag.sh (every pod's log since this run began as files, Postgres sessions and locks) runs
# first, before the release is restored: restoring replaces the pods and with them their logs (#416). That includes TERM,
# INT and HUP (a cancelled CI job): the slow children (helm --wait, kubectl wait / rollout status) are started in the
# background and waited for, because bash runs a trap only after a FOREGROUND child returns; the handler stops the child,
# the exit status is 128 + the signal, and the exit trap diagnoses, then restores (#420).
set -euo pipefail
. "$(dirname "$0")/../../../scripts/best-effort.sh"   # best_effort: diagnostics / cleanup / wait-gone; restore_step / restore_exit: restores (#462)
# refuses to run outside run.sh (#444): HOME is the runner's scratch directory, which holds the marker
[ "${KIND_RUNNER_HOME:-}" = "${HOME:-}" ] && [ -f "${HOME:-/nonexistent}/.kind-runner" ] || { echo "$0: run it through its task (task helm-kind-up / helm-kind / helm-kind-keda; CLUSTER=<name> picks the cluster): the task isolates it from ~/.kube/config (#444)" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
flow_start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
ns=$1 release=$2 chart=$3 values=$4 flow=$5
rport=${FLOW_RPC_PORT:-18094} sport=${FLOW_SERVE_PORT:-18095}
api="http://127.0.0.1:$rport/api" serve="http://127.0.0.1:$sport"
pfs=""
body=""
cur=""        # pid of the slow child being waited for (helm, kubectl wait / rollout)
finished=""   # set only when the flow ran to its end: any other way out is diagnosed
slow() { "$@" & cur=$!; local rc=0; wait "$cur" || rc=$?; cur=""; return "$rc"; }

fail() { echo "payment-flow: $*" >&2; best_effort kubectl -n "$ns" get pods,jobs >&2; exit 1; }   # diagnostic: fail() exits 1 whatever kubectl returns
restore() {
  for p in $pfs; do kill "$p" 2>/dev/null || true; done
  restore_step kubectl -n "$ns" scale "deploy/$release-payment-serve" "deploy/$release-payment-consume" --replicas=1 >/dev/null   # restore: a failure fails the run (restore_exit), not best effort (#462)
  restore_step helm upgrade --install "$release" "$chart" -n "$ns" -f "$values" --wait --timeout 3m >/dev/null
}
on_signal() { # <exit status>: stop the waited-for child, then leave through the exit trap (diagnose, restore)
  trap '' TERM INT HUP   # a second signal must not cut the diagnosis or the restore short
  # the child and ITS children (a wrapper around helm / kubectl leaves its own child behind otherwise)
  if [ -n "$cur" ]; then
    kids=$(pgrep -P "$cur" 2>/dev/null) || { pg_rc=$?; kids=""; [ "$pg_rc" -eq 1 ] || echo "payment-flow: pgrep -P $cur failed" >&2; }   # 1 = no children; explicit pids, never a pattern
    for c in $kids; do kill "$c" 2>/dev/null || true; done
    kill "$cur" 2>/dev/null || true; wait "$cur" 2>/dev/null || true; cur=""
  fi
  exit "$1"
}
trap 'on_signal 143' TERM
trap 'on_signal 130' INT
trap 'on_signal 129' HUP
on_exit() { # the script leaves with the status it had on the way in
  local rc=$?
  trap '' TERM INT HUP
  [ -n "$finished" ] || best_effort "$here/diag.sh" "$ns" "$flow_start" >&2   # diagnostic: on_exit leaves with the status it had
  restore
  restore_exit "$rc"   # the status it had on the way in; non-zero when only the restore failed
}
trap on_exit EXIT

up() { # up [extra helm args]: the release with the flow overlay, the switch as asked
  slow helm upgrade --install "$release" "$chart" -n "$ns" -f "$values" -f "$flow" "$@" --wait --timeout 4m >/dev/null || fail "helm upgrade did not become ready"
  forwards
}
forwards() {
  for p in $pfs; do kill "$p" 2>/dev/null || true; done
  pfs=""
  kubectl -n "$ns" port-forward "svc/$release-rpc" "$rport:80" >/dev/null 2>&1 & pfs="$pfs $!"
  kubectl -n "$ns" port-forward "svc/$release-payment-serve" "$sport:80" >/dev/null 2>&1 & pfs="$pfs $!"
  for ((_try76 = 1; _try76 <= 40; _try76++)); do curl -fsS "$api/readyz" >/dev/null 2>&1 && curl -fsS "$serve/api/readyz" >/dev/null 2>&1 && return 0; sleep 1; done
  fail "rpc / payment-rpc serve never answered readyz through the port-forward"
}

# the two credentials, from the overlay itself
shopper_key=$(sed -n 's/.*shopper=\([A-Za-z0-9._-]*\).*/\1/p' "$flow" | head -1)
admin_key=$(sed -n 's/.*admin=\([A-Za-z0-9._-]*\):admin.*/\1/p' "$flow" | head -1)
[ -n "$shopper_key" ] && [ -n "$admin_key" ] || fail "no shopper / admin key in $flow"

# call <key> <path> <json>: sets $res ("<http status> <x-error-code or ->") and $body in THIS shell, so it is called
# plainly (never inside $(...), whose subshell would keep them).
call() {
  local out rc=0
  out=$(curl -sS -D - -o /tmp/payflow.$$ -H 'Content-Type: application/json' -H "X-API-Key: $1" "$api/$2" -d "$3") || rc=$?
  if [ "$rc" != 0 ]; then echo "payment-flow: curl $2 failed (exit $rc): no status is read from it" >&2; out=""; fi   # an empty status never equals the expected one
  body=$(cat /tmp/payflow.$$ 2>/dev/null) || body="(no body)"; rm -f /tmp/payflow.$$
  local st xc   # each part on its own: call() runs in `$(…)` and `||` positions, so errexit is off in here
  st=$(sed -n '1s/^HTTP[^ ]* \([0-9]*\).*/\1/p' <<<"$out") || st=""   # an empty status never equals the expected one
  xc=$(tr -d '\r' <<<"$out" | sed -n 's/^[Xx]-[Ee]rror-[Cc]ode: //p' | head -n 1 | grep . || echo -) || xc=-
  res="$st $xc"
}
want() { [ "$2" = "$3" ] || fail "$1: got '$2', want '$3' (body: ${body:-})"; }
as_shopper() { call "$shopper_key" "$@"; }
as_admin() { call "$admin_key" "$@"; }
db() { kubectl -n "$ns" exec deploy/postgres -- psql -U app -d app -tAq -c "$1"; }

order_status() { as_shopper ordering.v1.OrderService/GetOrder "{\"id\":\"$1\"}" >/dev/null; jq -r '.order.status // "-"' <<<"$body"; }
wait_order() { # <order> <ORDER_STATUS_X> [seconds]
  local s=""
  for ((_try101 = 1; _try101 <= "${3:-60}"; _try101++)); do s=$(order_status "$1") || s="(status read failed, exit $?)"; [ "$s" = "$2" ] && return 0; sleep 1; done
  fail "order $1 is '$s' after ${3:-60}s, want $2"
}
steady() { # <order> <ORDER_STATUS_X> <seconds>: still this status for the whole window (the caller proved it could have moved)
  for ((_try105 = 1; _try105 <= "$3"; _try105++)); do [ "$(order_status "$1")" = "$2" ] || fail "order $1 left $2 within $3s"; sleep 1; done
}
pay_status() { as_shopper payment.v1.PublicPaymentService/GetPayment "{\"order_id\":\"$1\"}" >/dev/null; jq -r '.payment.status // "-"' <<<"$body"; }
wait_payment() { # <order> <PAYMENT_STATUS_X> [seconds]
  local s=""
  for ((_try110 = 1; _try110 <= "${3:-60}"; _try110++)); do s=$(pay_status "$1") || s="(status read failed, exit $?)"; [ "$s" = "$2" ] && return 0; sleep 1; done
  fail "payment of order $1 is '$s' after ${3:-60}s, want $2"
}
mock() { kubectl -n "$ns" exec "deploy/$release-payment-serve" -- /app mock "$@"; }
hook="http://127.0.0.1:8080/api/payment/webhook" # the webhook route, from inside the serve pod
refs() { db "SELECT provider_ref FROM payment.payments WHERE order_id = '$1'"; }
refunds() { db "SELECT count(*) FROM payment.mock_refunds WHERE ref = '$(refs "$1")'"; }
job() { # <cronjob> -> one run to completion
  local j ok bad rc
  j="flow-$1-$(date +%s)-$RANDOM"
  kubectl -n "$ns" create job "$j" --from="cronjob/$release-$1" >/dev/null
  for ((_try120 = 1; _try120 <= 90; _try120++)); do
    rc=0; ok=$(kubectl -n "$ns" get job "$j" -o jsonpath='{.status.succeeded}' 2>/dev/null) || rc=$?
    if [ "$rc" != 0 ]; then echo "payment-flow: reading job $j (succeeded) failed (exit $rc), retrying" >&2; ok=""; fi
    rc=0; bad=$(kubectl -n "$ns" get job "$j" -o jsonpath='{.status.failed}' 2>/dev/null) || rc=$?
    if [ "$rc" != 0 ]; then echo "payment-flow: reading job $j (failed) failed (exit $rc), retrying" >&2; bad=""; fi
    [ "${bad:-0}" != 0 ] && { best_effort kubectl -n "$ns" logs "job/$j" --tail=30; fail "$1 run failed"; }
    [ "$ok" = 1 ] && { kubectl -n "$ns" delete job "$j" >/dev/null; return 0; }
    sleep 2
  done
  fail "$1 run did not finish in 3 minutes"
}
payment_sweep() { job payment-expire; }

place() { # -> the id of an order that is CONFIRMED (stock seeded by the admin, ordered by the shopper)
  local sku r
  sku="flow-$(date +%s)-$RANDOM"
  as_admin catalog.v1.CatalogService/UpsertProduct "{\"sku\":\"$sku\",\"name\":\"$sku\",\"price\":{\"amount_minor\":100,\"currency\":\"USD\"}}"; want "UpsertProduct" "${res%% *}" 200
  as_admin inventory.v1.InventoryService/SetStock "{\"sku\":\"$sku\",\"on_hand\":10}"; want "SetStock" "${res%% *}" 200
  as_shopper ordering.v1.OrderService/CreateOrder "{\"customer_id\":\"shopper\",\"items\":[{\"sku\":\"$sku\",\"quantity\":1}]}"; want "CreateOrder" "${res%% *}" 200
  local id; id=$(jq -r .order.id <<<"$body")
  wait_order "$id" ORDER_STATUS_CONFIRMED
  printf '%s' "$id"
}
snapshot() { as_shopper ordering.v1.OrderService/GetOrder "{\"id\":\"$1\"}" >/dev/null; jq -r '.order.paymentRequired // false' <<<"$body"; }
new_order() { # -> a CONFIRMED order that needs payment (snapshot true)
  local id; id=$(place)
  want "payment_required snapshot of $id (switch on)" "$(snapshot "$id")" true
  printf '%s' "$id"
}
with_payment() { # <order>: wait for its payment (created by consume), then start it; echoes nothing
  wait_payment "$1" PAYMENT_STATUS_PENDING
  local r
  as_shopper payment.v1.PublicPaymentService/StartPayment "{\"order_id\":\"$1\"}"; want "StartPayment($1)" "${res%% *}" 200
  jq -e '.payment.checkoutUrl | startswith("https://mock-pay.invalid/checkout/")' <<<"$body" >/dev/null || fail "StartPayment returned no mock checkout address: $body"
}

# ── the release, the switch on ─────────────────────────────────────────────────────────────────────────────
up
echo "payment-flow: the release runs with ordering.require_payment on, auth required, the mock control on"

# 1 ── create → CONFIRMED → StartPayment → pay → webhook → COMPLETED ───────────────────────────────────────
o=$(new_order); with_payment "$o"
as_shopper ordering.v1.OrderService/CompleteOrder "{\"id\":\"$o\"}"; want "CompleteOrder of an order that needs payment" "$res" "400 order.payment_required"
out=$(mock pay --order "$o" --deliver "$hook") || fail "mock pay"
[ "$(jq -r .delivered_status <<<"$out")" = 200 ] || fail "the webhook was not accepted: $out"
wait_order "$o" ORDER_STATUS_COMPLETED
want "payment after the order completed" "$(pay_status "$o")" PAYMENT_STATUS_CAPTURED
echo "payment-flow 1: $o  CONFIRMED -> StartPayment -> paid -> webhook -> COMPLETED (CompleteOrder by hand refused: order.payment_required)"

# 3 ── redelivery: the same notification again and again ──────────────────────────────────────────────────
ev=$(jq -r .event_id <<<"$out")
for _ in 1 2 3; do mock notify --ref "$(refs "$o")" --id "$ev" --type payment.captured --deliver "$hook" >/dev/null || fail "redelivery"; done
want "webhook rows for one event after redelivery" "$(db "SELECT count(*) FROM payment.webhook_events WHERE provider_event_id = '$ev'")" 1
want "order after redelivery" "$(order_status "$o")" ORDER_STATUS_COMPLETED
want "payment after redelivery" "$(pay_status "$o")" PAYMENT_STATUS_CAPTURED
echo "payment-flow 3: three redeliveries of the same notification: one row, order COMPLETED, payment CAPTURED"

# 4 ── out of order / foreign ──────────────────────────────────────────────────────────────────────────────
mock notify --ref "$(refs "$o")" --id "evt-unknown-$$" --type payment.some_future_event --amount 100 --currency USD --deliver "$hook" >/dev/null || fail "an unknown event type must be accepted (and ignored)"
mock notify --ref "no-such-checkout-$$" --id "evt-orphan-$$" --type payment.captured --amount 100 --currency USD --deliver "$hook" >/dev/null || fail "a notification for a checkout nobody owns must be accepted (and parked)"
want "order after foreign notifications" "$(order_status "$o")" ORDER_STATUS_COMPLETED
echo "payment-flow 4: an unknown event type and a notification for a checkout nobody owns change nothing"

# 2 ── delayed webhook, and the webhook that never comes ───────────────────────────────────────────────────
o2=$(new_order); with_payment "$o2"
d=$(mock pay --order "$o2") || fail "mock pay (no delivery)"
want "paid at the provider" "$(db "SELECT paid FROM payment.mock_checkouts WHERE ref = '$(refs "$o2")'")" t
steady "$o2" ORDER_STATUS_CONFIRMED 8
want "payment while the webhook is late" "$(pay_status "$o2")" PAYMENT_STATUS_PENDING
curl -fsS -X POST -H "X-Payment-Signature: $(jq -r .signature <<<"$d")" --data-binary "$(jq -r .body <<<"$d")" "$serve/api/payment/webhook" >/dev/null || fail "late webhook refused"
wait_order "$o2" ORDER_STATUS_COMPLETED
o3=$(new_order); with_payment "$o3"
mock pay --order "$o3" >/dev/null || fail "mock pay (the webhook that never comes)"
db "UPDATE payment.payments SET expires_at = now() - interval '5 minutes' WHERE order_id = '$o3'" >/dev/null
payment_sweep
wait_order "$o3" ORDER_STATUS_COMPLETED
want "payment found by the sweep" "$(pay_status "$o3")" PAYMENT_STATUS_CAPTURED
echo "payment-flow 2: a late webhook completes the order; with no webhook at all the expire sweep finds the money (T6) and completes it"

# 5 ── the customer cancels while paying; the money arrives late ───────────────────────────────────────────
o=$(new_order); with_payment "$o"
as_shopper ordering.v1.OrderService/CancelOrder "{\"id\":\"$o\"}"; want "CancelOrder" "${res%% *}" 200
wait_order "$o" ORDER_STATUS_CANCELLED
wait_payment "$o" PAYMENT_STATUS_CANCELLED
want "the checkout was cancelled at the provider (T7)" "$(db "SELECT cancelled FROM payment.mock_checkouts WHERE ref = '$(refs "$o")'")" t
mock pay --order "$o" --deliver "$hook" >/dev/null || fail "late payment"
wait_payment "$o" PAYMENT_STATUS_REFUNDED
want "refunds of the late payment" "$(refunds "$o")" 1
want "order after the late payment" "$(order_status "$o")" ORDER_STATUS_CANCELLED
echo "payment-flow 5: $o cancelled mid-payment, money arrived anyway: payment REFUNDED once, order stays CANCELLED"

# 6 ── the window runs out; the money arrives late ──────────────────────────────────────────────────────────
o=$(new_order); with_payment "$o"
db "UPDATE payment.payments SET expires_at = now() - interval '5 minutes' WHERE order_id = '$o'" >/dev/null
payment_sweep
wait_order "$o" ORDER_STATUS_CANCELLED
mock pay --order "$o" --deliver "$hook" >/dev/null || fail "late payment"
wait_payment "$o" PAYMENT_STATUS_REFUNDED
want "order after the late payment" "$(order_status "$o")" ORDER_STATUS_CANCELLED
want "refunds" "$(refunds "$o")" 1
echo "payment-flow 6: $o expired (PaymentExpired cancelled it), money arrived late: REFUNDED once, order stays CANCELLED"

# 7 ── the TTL sequence with the provider down ──────────────────────────────────────────────────────────────
o=$(new_order); with_payment "$o"
mock pay --order "$o" >/dev/null || fail "mock pay (webhook lost)"
mock fail on >/dev/null || fail "mock fail on"
db "UPDATE inventory.reservations SET created_at = now() - interval '25 hours', updated_at = now() - interval '25 hours' WHERE order_id = '$o'" >/dev/null
job expire
wait_order "$o" ORDER_STATUS_CANCELLED
wait_payment "$o" PAYMENT_STATUS_CANCELLED
payment_sweep
steady "$o" ORDER_STATUS_CANCELLED 3
want "payment while the provider is down" "$(pay_status "$o")" PAYMENT_STATUS_CANCELLED
want "refunds while the provider is down" "$(refunds "$o")" 0
mock fail off >/dev/null || fail "mock fail off"
payment_sweep
wait_payment "$o" PAYMENT_STATUS_REFUNDED
want "refunds after recovery" "$(refunds "$o")" 1
want "order after recovery" "$(order_status "$o")" ORDER_STATUS_CANCELLED
echo "payment-flow 7: reservation TTL -> cancelled; provider down: sweeps change nothing; recovered: money found (T8), REFUNDED once, order stays CANCELLED"

# 8 ── the switch (docs/payment.md §8: ordering looks at the order's snapshot, never at the current switch) ────────
# on -> off: orders that became CONFIRMED under the switch are not touched by turning it off
on_started=$(new_order); with_payment "$on_started"   # a checkout is open
on_idle=$(new_order); wait_payment "$on_idle" PAYMENT_STATUS_PENDING   # payment exists, nobody started it
up --set config.ordering.require_payment=false
as_shopper ordering.v1.OrderService/CompleteOrder "{\"id\":\"$on_started\"}"; want "CompleteOrder of a snapshot-true order, switch off" "$res" "400 order.payment_required"
as_shopper payment.v1.PublicPaymentService/StartPayment "{\"order_id\":\"$on_idle\"}"; want "StartPayment, switch off" "$res" "400 payment.disabled"
mock pay --order "$on_started" --deliver "$hook" >/dev/null || fail "pay after the switch was turned off"
wait_order "$on_started" ORDER_STATUS_COMPLETED
db "UPDATE payment.payments SET expires_at = now() - interval '5 minutes' WHERE order_id = '$on_idle'" >/dev/null
payment_sweep
wait_order "$on_idle" ORDER_STATUS_CANCELLED
# orders confirmed with the switch off take snapshot false
off_a=$(place); off_b=$(place); off_c=$(place)
for o in "$off_a" "$off_b" "$off_c"; do want "payment_required snapshot of $o (switch off)" "$(snapshot "$o")" false; done
as_shopper ordering.v1.OrderService/CompleteOrder "{\"id\":\"$off_a\"}"; want "CompleteOrder of a snapshot-false order, switch off" "${res%% *}" 200
# off -> on: orders from before the switch came on are not touched by turning it on
up
as_shopper ordering.v1.OrderService/CompleteOrder "{\"id\":\"$off_b\"}"; want "CompleteOrder of a snapshot-false order, switch on" "${res%% *}" 200
as_shopper payment.v1.PublicPaymentService/StartPayment "{\"order_id\":\"$off_c\"}"; want "StartPayment of a snapshot-false order, switch on" "$res" "400 payment.disabled"
wait_order "$off_b" ORDER_STATUS_COMPLETED
echo "payment-flow 8: switch on->off: the CONFIRMED order with an open checkout is paid to COMPLETED, the one never started is cancelled by the sweep, CompleteOrder is refused (order.payment_required) and StartPayment says payment.disabled; off->on: orders confirmed while it was off complete by hand and cannot be paid (payment.disabled)"

# 9 ── payment-rpc stopped ───────────────────────────────────────────────────────────────────────────────
kubectl -n "$ns" scale "deploy/$release-payment-serve" "deploy/$release-payment-consume" --replicas=0 >/dev/null
best_effort slow kubectl -n "$ns" wait --for=delete pod -l "app.kubernetes.io/component in (payment-serve,payment-consume)" --timeout=2m >/dev/null   # wait-gone: "no pods left" is the state wanted; the steps after it assert
rpc_pods_before=$(kubectl -n "$ns" get pods -l app.kubernetes.io/component=rpc -o jsonpath='{range .items[*]}{.metadata.name}:{.status.containerStatuses[0].restartCount} {end}')
sku="flow-down-$(date +%s)"
as_admin catalog.v1.CatalogService/UpsertProduct "{\"sku\":\"$sku\",\"name\":\"$sku\",\"price\":{\"amount_minor\":100,\"currency\":\"USD\"}}" >/dev/null
as_admin inventory.v1.InventoryService/SetStock "{\"sku\":\"$sku\",\"on_hand\":10}" >/dev/null
as_shopper ordering.v1.OrderService/CreateOrder "{\"customer_id\":\"shopper\",\"items\":[{\"sku\":\"$sku\",\"quantity\":1}]}"; want "CreateOrder with payment-rpc stopped" "${res%% *}" 200
od=$(jq -r .order.id <<<"$body")
wait_order "$od" ORDER_STATUS_CONFIRMED
as_shopper payment.v1.PublicPaymentService/StartPayment "{\"order_id\":\"$od\"}"; want "StartPayment with payment-rpc stopped" "$res" "503 payment.unavailable"
kubectl -n "$ns" scale "deploy/$release-payment-serve" "deploy/$release-payment-consume" --replicas=1 >/dev/null
slow kubectl -n "$ns" rollout status "deploy/$release-payment-serve" "deploy/$release-payment-consume" --timeout=2m >/dev/null || fail "payment-rpc did not come back"
forwards
with_payment "$od"   # consume turns the OrderConfirmed that waited into the payment; the rpc pods were never restarted
out=$(mock pay --order "$od" --deliver "$hook") || fail "mock pay after recovery"
wait_order "$od" ORDER_STATUS_COMPLETED
want "rpc pods across the outage (names and restarts)" "$(kubectl -n "$ns" get pods -l app.kubernetes.io/component=rpc -o jsonpath='{range .items[*]}{.metadata.name}:{.status.containerStatuses[0].restartCount} {end}')" "$rpc_pods_before"
echo "payment-flow 9: payment-rpc stopped: the order was reserved and CONFIRMED, StartPayment answered 503 payment.unavailable; back again, the same rpc pods paid it to COMPLETED"

finished=1
echo "payment-flow ok"
