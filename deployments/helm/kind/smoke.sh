#!/usr/bin/env bash
# End-to-end through the installed release: rpc answers, an order reserves stock via the worker,
# the console serves the UI and proxies /api to the rpc.
#   smoke.sh <namespace> <release>
# Needs kubectl + curl + jq. Exits non-zero (with the pods' state) when anything is off.
set -euo pipefail
ns=$1 release=$2 port=${SMOKE_PORT:-18080} cport=${SMOKE_CONSOLE_PORT:-18081}
api="http://127.0.0.1:$port/api"
console="http://127.0.0.1:$cport"

kubectl -n "$ns" port-forward "svc/$release-rpc" "$port:80" >/dev/null 2>&1 &
pf=$!
kubectl -n "$ns" port-forward "svc/$release-console" "$cport:80" >/dev/null 2>&1 &
pfc=$!
trap 'kill $pf $pfc 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -fsS "$api/readyz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "$api/readyz" >/dev/null || { echo "smoke: /api/readyz never answered 200"; exit 1; }
echo "smoke: running $(curl -fsS "$api/version")"

# Console: the SPA for any route (TanStack Router handles /reservations client-side), the rpc
# through nginx's /api proxy — what the browser does.
for _ in $(seq 1 30); do curl -fsS "$console/healthz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "$console/reservations" | grep -q "<title>x-monorepo console</title>" \
  || { echo "smoke: console did not serve index.html for /reservations"; exit 1; }
curl -fsSI "$console/reservations" | grep -qi '^content-security-policy: .*frame-ancestors' \
  || { echo "smoke: console index.html lacks the CSP header"; exit 1; }
curl -fsSI "$console/api/readyz" | grep -qi '^content-security-policy' \
  && { echo "smoke: console applied its CSP to a proxied /api response"; exit 1; }
curl -fsS "$console/api/readyz" >/dev/null || { echo "smoke: console /api proxy to the rpc failed"; exit 1; }
echo "smoke: console serves the UI and proxies /api → $(curl -fsS "$console/api/version")"

# Dex (dex.yaml): the issuer URL the rpc's auth.oidc names must resolve inside the cluster (the
# CoreDNS rewrite) and its discovery document must name exactly that issuer.
issuer=http://dex.localtest.me:5556/dex
got=$(kubectl -n "$ns" exec deploy/dex -- wget -qO- "$issuer/.well-known/openid-configuration" | jq -r .issuer) \
  || { echo "smoke: $issuer is not reachable from inside the cluster (CoreDNS rewrite?)"; exit 1; }
[ "$got" = "$issuer" ] || { echo "smoke: dex discovery issuer is '$got', want '$issuer'"; exit 1; }
echo "smoke: dex discovery resolves in-cluster as $issuer"

rpc() { curl -fsS -H 'Content-Type: application/json' "$api/$1" -d "$2"; }
sku="kind-$(date +%s)"
rpc inventory.v1.InventoryService/SetStock "{\"sku\":\"$sku\",\"on_hand\":10}" >/dev/null
# CreateOrder prices its lines from the catalog: the SKU has to be a product first.
rpc catalog.v1.CatalogService/UpsertProduct "{\"sku\":\"$sku\",\"name\":\"$sku\",\"price\":{\"amount_minor\":100,\"currency\":\"USD\"}}" >/dev/null
order=$(rpc ordering.v1.OrderService/CreateOrder \
  "{\"customer_id\":\"smoke\",\"items\":[{\"sku\":\"$sku\",\"quantity\":3}]}" \
  | jq -r .order.id)
[ -n "$order" ] && [ "$order" != null ] || { echo "smoke: CreateOrder returned no id"; exit 1; }

# OrderCreated → outbox → forwarder → Redis Streams → inventory consumer → stock.reserved.
for _ in $(seq 1 30); do
  reserved=$(rpc inventory.v1.InventoryService/GetStock "{\"sku\":\"$sku\"}" | jq -r '.stock.reserved // 0')
  [ "$reserved" = 3 ] && { echo "smoke ok: order $order reserved 3 of $sku through the worker"; exit 0; }
  sleep 1
done
echo "smoke: stock $sku reserved=$reserved after 30s, want 3"
kubectl -n "$ns" get pods
kubectl -n "$ns" logs -l app.kubernetes.io/component=worker --tail=50
exit 1
