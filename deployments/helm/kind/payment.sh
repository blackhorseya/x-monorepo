#!/usr/bin/env bash
# payment-rpc through the installed release (paymentRpc.enabled in kind/values.yaml):
#   payment.sh <namespace> <release>
# 1. the three workloads: serve answers (the service key is required), consume runs, an expire CronJob run succeeds;
# 2. the Ingress (ingress-nginx, installed here): the provider's webhook reaches payment-rpc, and nothing else does —
#    not the Connect RPC, not /api/metrics, not the probes — and the rate limit really limits;
# 3. payment-rpc stopped: an order still reaches CONFIRMED (nothing in the order flow waits for payment).
# Needs kubectl + helm + curl + jq. Exits non-zero (with the pods' state) when anything is off. Restores the release.
set -euo pipefail
ns=$1 release=$2
ingress_chart_version=${INGRESS_NGINX_VERSION:-4.14.3}
host=pay.localtest.me
sport=${PAYMENT_SERVE_PORT:-18090} iport=${PAYMENT_INGRESS_PORT:-18091} rport=${PAYMENT_RPC_PORT:-18092}

pf=""
trap 'test -z "$pf" || kill $pf 2>/dev/null || true' EXIT
fail() { echo "payment: $*"; kubectl -n "$ns" get pods,cronjobs,jobs,ingress 2>/dev/null || true; exit 1; }
forward() { # forward <target> <local port> <remote port>
  kubectl "${@:4}" port-forward "$1" "$2:$3" >/dev/null 2>&1 &
  pf="$pf $!"
}
code() { curl -s -o /dev/null -w '%{http_code}' "$@" || echo 000; }

# ── 1. the workloads ───────────────────────────────────────────────────────────────────────────────
kubectl -n "$ns" rollout status "deploy/$release-payment-serve" "deploy/$release-payment-consume" --timeout=2m >/dev/null \
  || fail "serve / consume did not roll out"
for what in deploy/$release-payment-serve deploy/$release-payment-consume cronjob/$release-payment-expire; do
  img=$(kubectl -n "$ns" get "$what" -o jsonpath='{.spec.template.spec.containers[0].image}{.spec.jobTemplate.spec.template.spec.containers[0].image}')
  case $img in */payment-rpc:*) ;; *) fail "$what runs '$img', not the payment-rpc image" ;; esac
done
cfg=$(kubectl -n "$ns" get configmap "$release-payment-rpc" -o jsonpath='{.data.config\.yaml}')
# grep -q under pipefail: a pipeline whose reader exits at the first match can fail with SIGPIPE on the writer (CI did,
# at the rpc env check below), so every check reads a variable (here-string), never a pipe.
grep -q 'oidc\|jwt' <<<"$cfg" && fail "payment-rpc's ConfigMap carries auth.jwt / auth.oidc (this release's rpc has OIDC on: it must not inherit it)"
grep -q 'mode: required' <<<"$cfg" || fail "payment-rpc's ConfigMap is not auth.mode required"
grep -q 'broker: postgres' <<<"$cfg" || fail "payment-rpc's ConfigMap is not the Postgres broker (this release's worker uses Redis; consume reads the outbox tables)"
key=$(kubectl -n "$ns" get secret "$release-payment-rpc-env" -o jsonpath='{.data.APP_PAYMENT_API_KEY}' | base64 -d)
auth=$(kubectl -n "$ns" get secret "$release-payment-rpc-env" -o jsonpath='{.data.APP_AUTH_API_KEYS}' | base64 -d)
[ "$auth" = "rpc=$key:admin" ] || fail "payment-rpc's service key is not exactly rpc=<the key rpc presents>:admin"
# who is told what: rpc has the address and the key, nobody has the provider credential but the three payment workloads
rpcenv=$(kubectl -n "$ns" get deploy "$release-rpc" -o jsonpath='{.spec.template.spec.containers[0].env[*].name}')
case " $rpcenv " in *" APP_PAYMENT_UPSTREAM "*) ;; *) fail "rpc was not told where payment-rpc is" ;; esac
for w in rpc worker catalog-rpc; do
  names=$(kubectl -n "$ns" get deploy "$release-$w" -o jsonpath='{.spec.template.spec.containers[*].env[*].name}')
  case " $names " in *" APP_PAYMENT_MOCK_WEBHOOK_SECRET "*) fail "$w holds the provider credential" ;; esac
done
for _ in $(seq 1 30); do
  clog=$(kubectl -n "$ns" logs "deploy/$release-payment-consume" 2>/dev/null || true)
  grep -q 'payment-rpc consume started' <<<"$clog" && break; sleep 1
done
grep -q 'payment-rpc consume started' <<<"$clog" || fail "consume never logged that it started"

forward "svc/$release-payment-serve" "$sport" 80 -n "$ns"
direct="http://127.0.0.1:$sport"
for _ in $(seq 1 30); do [ "$(code "$direct/api/readyz")" = 200 ] && break; sleep 1; done
[ "$(code "$direct/api/readyz")" = 200 ] || fail "payment-rpc serve /api/readyz never answered 200"
body='{"orderId":"kind-payment-order","customerId":"kind-payment-customer"}'
[ "$(code -X POST -H 'Content-Type: application/json' -d "$body" "$direct/payment.v1.PaymentService/GetPayment")" = 401 ] \
  || fail "payment-rpc answered a call with no service key"
[ "$(code -X POST -H 'Content-Type: application/json' -H 'X-API-Key: not-the-service-key-0123456789' -d "$body" "$direct/payment.v1.PaymentService/GetPayment")" = 401 ] \
  || fail "payment-rpc accepted an unknown key"
[ "$(code -X POST -H 'Content-Type: application/json' -H "X-API-Key: $key" -d "$body" "$direct/payment.v1.PaymentService/GetPayment")" = 404 ] \
  || fail "payment-rpc refused its own service key (want 404 payment.not_found for an unknown order)"
echo "payment: serve is ready; the RPC wants the service key (401 without, 401 unknown, 404 not_found with)"

# the sweep: one run of the CronJob, to completion
job="payment-expire-kind-$(date +%s)"
kubectl -n "$ns" create job "$job" --from="cronjob/$release-payment-expire" >/dev/null
done_=""
for _ in $(seq 1 90); do
  ok=$(kubectl -n "$ns" get job "$job" -o jsonpath='{.status.succeeded}' 2>/dev/null || true)
  bad=$(kubectl -n "$ns" get job "$job" -o jsonpath='{.status.failed}' 2>/dev/null || true)
  [ "${bad:-0}" != 0 ] && { kubectl -n "$ns" logs "job/$job" --tail=30 || true; fail "the expire run failed"; }
  [ "$ok" = 1 ] && { done_=1; break; }
  sleep 2
done
[ -n "$done_" ] || fail "the expire run did not finish in 3 minutes"
echo "payment: an expire run succeeded ($job)"
kubectl -n "$ns" delete job "$job" >/dev/null

# ── 2. the Ingress ─────────────────────────────────────────────────────────────────────────────────
# The controller is not part of the chart; install it the way a cluster would (the admission webhook off: the
# release's Ingress is applied right after, with no webhook to wait for).
helm upgrade --install ingress-nginx ingress-nginx --repo https://kubernetes.github.io/ingress-nginx \
  --version "$ingress_chart_version" -n ingress-nginx --create-namespace \
  --set controller.ingressClassResource.name=nginx --set controller.service.type=ClusterIP \
  --set controller.admissionWebhooks.enabled=false --wait --timeout 4m >/dev/null \
  || fail "could not install ingress-nginx"
forward svc/ingress-nginx-controller "$iport" 80 -n ingress-nginx
ing="http://127.0.0.1:$iport"
# hit <method> <path> [curl args]: the status the ingress answers for the webhook host. The limit counts EVERY
# request to the host, routed or not (an unrouted path past the burst is a 503, not a 404: observed), so the
# checks below that expect a 404 are paced under rps 2.
hit() {
  local m=$1 p=$2; shift 2
  sleep 0.6
  code -X "$m" -H "Host: $host" "$@" "$ing$p"
}
# wait for the controller to have the rule: the webhook answers 400 (payment-rpc: not signed), not nginx's 404
for _ in $(seq 1 60); do [ "$(hit POST /api/payment/webhook -d '{}')" = 400 ] && break; sleep 2; done
[ "$(hit POST /api/payment/webhook -d '{}')" = 400 ] || fail "the webhook never reached payment-rpc through the ingress (want 400: unsigned)"
echo "payment: POST /api/payment/webhook reaches payment-rpc through the ingress (400: not signed)"

# What must NOT be reachable from outside. Each of these answers 200/401 on the Service itself (direct, port-forward),
# so a 404 at the ingress is the ingress not routing it, not the backend not having it.
[ "$(code "$direct/api/metrics")" = 200 ] && [ "$(code "$direct/api/healthz")" = 200 ] || fail "control: metrics / healthz are not on the Service itself"
for pth in /api/metrics /api/readyz /api/healthz /api/version /api/docs /api/payment/webhook/ /api/payment/webhookx /api/payment/; do
  [ "$(hit GET "$pth")" = 404 ] || fail "GET $pth is routed by the ingress (want 404)"
done
for pth in /payment.v1.PaymentService/GetPayment /api/payment.v1.PaymentService/GetPayment /payment.v1.PaymentService/StartPayment /api/payment.v1.PaymentService/StartPayment; do
  c=$(hit POST "$pth" -H 'Content-Type: application/json' -H "X-API-Key: $key" -d "$body")
  [ "$c" = 404 ] || fail "POST $pth (even with the service key) is routed by the ingress: $c, want 404"
done
sleep 0.6
[ "$(code -H "Host: other.localtest.me" -X POST -d '{}' "$ing/api/payment/webhook")" = 404 ] || fail "the webhook answers for a host that is not paymentRpc.ingress.hosts"
echo "payment: the ingress routes the webhook path and nothing else (RPC, metrics, probes, docs, other hosts: 404)"

# The rate limit: rps 2 x burst multiplier 2 per client. A fast burst has to be refused (503) and the
# first requests answered by payment-rpc (400); a moment later it recovers.
sleep 3
res=$(seq 1 40 | xargs -P 20 -I{} curl -sS -o /dev/null -w '%{http_code}\n' -X POST -H "Host: $host" -d '{}' "$ing/api/payment/webhook")
n400=$(grep -c '^400$' <<<"$res" || true) n503=$(grep -c '^503$' <<<"$res" || true)
[ "$n400" -ge 1 ] || fail "burst: no request reached payment-rpc ($n400 x 400, $n503 x 503)"
[ "$n503" -ge 1 ] || fail "burst: 40 requests in a burst were never limited ($n400 x 400, $n503 x 503): the rate limit is not in force"
sleep 5
[ "$(hit POST /api/payment/webhook -d '{}')" = 400 ] || fail "the limit did not release after the burst"
echo "payment: the rate limit is in force ($n400 x 400, $n503 x 503 in a burst of 40) and releases"
big=$(mktemp)
head -c 2097152 /dev/zero | tr '\0' 'a' >"$big"
[ "$(hit POST /api/payment/webhook --data-binary "@$big")" = 413 ] || fail "a 2 MiB body was not refused with 413"
rm -f "$big"
echo "payment: a 2 MiB webhook body is refused at the ingress (413)"

# ── 3. payment-rpc stopped: ordering is unaffected ─────────────────────────────────────────────────
restore() { kubectl -n "$ns" scale "deploy/$release-payment-serve" "deploy/$release-payment-consume" --replicas=1 >/dev/null 2>&1 || true; }
trap 'restore; test -z "$pf" || kill $pf 2>/dev/null || true' EXIT
kubectl -n "$ns" scale "deploy/$release-payment-serve" "deploy/$release-payment-consume" --replicas=0 >/dev/null
kubectl -n "$ns" wait --for=delete pod -l "app.kubernetes.io/component in (payment-serve,payment-consume)" --timeout=2m >/dev/null 2>&1 || true
[ "$(hit POST /api/payment/webhook -d '{}')" != 400 ] || fail "control: the webhook still reaches payment-rpc with it stopped"
forward "svc/$release-rpc" "$rport" 80 -n "$ns"
api="http://127.0.0.1:$rport/api"
for _ in $(seq 1 30); do [ "$(code "$api/readyz")" = 200 ] && break; sleep 1; done
rpc() { curl -fsS -H 'Content-Type: application/json' "$api/$1" -d "$2"; }
sku="kind-pay-$(date +%s)"
rpc catalog.v1.CatalogService/UpsertProduct "{\"sku\":\"$sku\",\"name\":\"$sku\",\"price\":{\"amount_minor\":100,\"currency\":\"USD\"}}" >/dev/null
rpc inventory.v1.InventoryService/SetStock "{\"sku\":\"$sku\",\"on_hand\":10}" >/dev/null
order=$(rpc ordering.v1.OrderService/CreateOrder "{\"customer_id\":\"payment-smoke\",\"items\":[{\"sku\":\"$sku\",\"quantity\":1}]}" | jq -r .order.id)
[ -n "$order" ] && [ "$order" != null ] || fail "CreateOrder returned no id with payment-rpc stopped"
status=""
for _ in $(seq 1 60); do
  status=$(rpc ordering.v1.OrderService/GetOrder "{\"id\":\"$order\"}" | jq -r .order.status)
  [ "$status" = ORDER_STATUS_CONFIRMED ] && break
  sleep 1
done
[ "$status" = ORDER_STATUS_CONFIRMED ] || fail "with payment-rpc stopped, order $order is '$status' after 60s, want ORDER_STATUS_CONFIRMED"
echo "payment: with payment-rpc stopped, order $order still reaches CONFIRMED"

restore
kubectl -n "$ns" rollout status "deploy/$release-payment-serve" "deploy/$release-payment-consume" --timeout=2m >/dev/null || fail "payment-rpc did not come back"
echo "payment ok"
