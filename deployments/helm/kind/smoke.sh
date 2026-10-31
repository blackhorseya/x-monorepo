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

# catalog-rpc (catalogRpc.enabled in kind/values.yaml): its own pod, ConfigMap and Secret. It refuses to
# start on auth.jwt / auth.oidc.enabled, and this release's rpc config has auth.oidc on, so a pod
# that is Ready proves it did not inherit that. Auth is required with ONE static key, rpc=<key>:admin.
cport_cat=${SMOKE_CATALOG_PORT:-18082}
kubectl -n "$ns" port-forward "svc/$release-catalog-rpc" "$cport_cat:80" >/dev/null 2>&1 &
pfk=$!
trap 'kill $pf $pfc $pfk 2>/dev/null || true' EXIT
cat="http://127.0.0.1:$cport_cat/api"
for _ in $(seq 1 30); do curl -fsS "$cat/readyz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "$cat/readyz" >/dev/null || { echo "smoke: catalog-rpc /api/readyz never answered 200"; exit 1; }
curl -fsS "$cat/version" >/dev/null || { echo "smoke: catalog-rpc /api/version failed"; exit 1; }
catimg=$(kubectl -n "$ns" get deploy -l app.kubernetes.io/component=catalog-rpc -o jsonpath='{.items[0].spec.template.spec.containers[0].image}')
case $catimg in */catalog-rpc:*) ;; *) echo "smoke: the catalog-rpc Deployment runs '$catimg', not the catalog-rpc image"; exit 1 ;; esac
echo "smoke: catalog-rpc running $catimg $(curl -fsS "$cat/version")"
catcfg=$(kubectl -n "$ns" get configmap -l app.kubernetes.io/component=catalog-rpc -o jsonpath='{.items[0].data.config\.yaml}')
echo "$catcfg" | grep -q 'oidc\|jwt' && { echo "smoke: catalog-rpc ConfigMap carries auth.jwt/auth.oidc"; exit 1; }
echo "$catcfg" | grep -q 'mode: required' || { echo "smoke: catalog-rpc ConfigMap is not auth.mode required"; exit 1; }
catkey=$(kubectl -n "$ns" get secret -l app.kubernetes.io/component=catalog-rpc -o jsonpath='{.items[0].data.APP_AUTH_API_KEYS}' | base64 -d)
case $catkey in rpc=*:admin) catkey=${catkey#rpc=}; catkey=${catkey%:admin} ;; *) echo "smoke: catalog-rpc service key is not rpc=<key>:admin"; exit 1 ;; esac
catrpc() { curl -sS -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' ${3:+-H "X-API-Key: $3"} "$cat/$1" -d "$2"; }
catsku="kind-cat-$(date +%s)"
up="{\"sku\":\"$catsku\",\"name\":\"$catsku\",\"price\":{\"amount_minor\":250,\"currency\":\"USD\"}}"
[ "$(catrpc catalog.v1.CatalogService/UpsertProduct "$up")" = 401 ] || { echo "smoke: catalog-rpc accepted a write with no credential"; exit 1; }
[ "$(catrpc catalog.v1.CatalogService/GetProduct "{\"sku\":\"$catsku\"}")" = 401 ] || { echo "smoke: catalog-rpc accepted a read with no credential"; exit 1; }
[ "$(catrpc catalog.v1.CatalogService/UpsertProduct "$up" not-the-service-key-0123456789)" = 401 ] || { echo "smoke: catalog-rpc accepted an unknown key"; exit 1; }
[ "$(catrpc catalog.v1.CatalogService/UpsertProduct "$up" "$catkey")" = 200 ] || { echo "smoke: catalog-rpc refused its own service key"; exit 1; }
# What catalog-rpc wrote is what the rpc serves: this GetProduct hits the rpc, whose Forwarder passes
# it to catalog-rpc over Connect. (The rpc has no catalog repository of its own, so a 404 here means
# the Forwarder or catalog-rpc is broken.)
got=$(curl -fsS -H 'Content-Type: application/json' "$api/catalog.v1.CatalogService/GetProduct" -d "{\"sku\":\"$catsku\"}" | jq -r '.product.price.amountMinor // .product.price.amount_minor')
[ "$got" = 250 ] || { echo "smoke: rpc did not read catalog-rpc's write back from Postgres (got '$got')"; exit 1; }
echo "smoke: catalog-rpc requires its service key (401 without, 401 unknown, 200 with) and persists to the shared Postgres"

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
# The SKU has to be a catalog product first: SetStock refuses one the catalog does not have
# (stock.unknown_sku), and CreateOrder prices its lines from the catalog.
rpc catalog.v1.CatalogService/UpsertProduct "{\"sku\":\"$sku\",\"name\":\"$sku\",\"price\":{\"amount_minor\":100,\"currency\":\"USD\"}}" >/dev/null
rpc inventory.v1.InventoryService/SetStock "{\"sku\":\"$sku\",\"on_hand\":10}" >/dev/null
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
