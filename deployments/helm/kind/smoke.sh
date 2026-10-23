#!/usr/bin/env bash
# End-to-end through the installed release: rpc answers, an order reserves stock via the worker.
#   smoke.sh <namespace> <release>
# Needs kubectl + curl + jq. Exits non-zero (with the pods' state) when anything is off.
set -euo pipefail
ns=$1 release=$2 port=${SMOKE_PORT:-18080}
api="http://127.0.0.1:$port/api"

kubectl -n "$ns" port-forward "svc/$release-rpc" "$port:80" >/dev/null 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -fsS "$api/readyz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "$api/readyz" >/dev/null || { echo "smoke: /api/readyz never answered 200"; exit 1; }
echo "smoke: running $(curl -fsS "$api/version")"

rpc() { curl -fsS -H 'Content-Type: application/json' "$api/$1" -d "$2"; }
sku="kind-$(date +%s)"
rpc inventory.v1.InventoryService/SetStock "{\"sku\":\"$sku\",\"on_hand\":10}" >/dev/null
order=$(rpc ordering.v1.OrderService/CreateOrder \
  "{\"customer_id\":\"smoke\",\"items\":[{\"sku\":\"$sku\",\"quantity\":3,\"unit_price\":{\"amount_minor\":100,\"currency\":\"USD\"}}]}" \
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
