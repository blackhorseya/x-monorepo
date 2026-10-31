#!/usr/bin/env bash
# rpc without its catalog, on the installed release (#320 slice 4). Two things the chart promises:
#   A. catalog-rpc DOWN, rpc rolls out: the initContainer waits its whole budget, logs a WARN, and the rpc pod comes up
#      anyway and Ready. CreateOrder and SetStock answer UNAVAILABLE (503, x-error-code catalog.unavailable) and write
#      nothing, every other API and rpc's readyz are untouched. When the catalog comes back the SAME rpc pod works again,
#      never restarted.
#   B. catalog-rpc comes up DURING the wait: the wait ends before its budget, with no warning (the normal upgrade order:
#      the catalog first, then the rpc takes traffic).
# Best effort is the contract: the wait never holds a rollout, so a catalog outage never takes the API down with it.
# The release is restored (helm upgrade --wait) on the way out, whatever happens.
#   outage.sh <namespace> <release> <chart-dir> <values-file>
set -euo pipefail
ns=$1 release=$2 chart=$3 values=$4
rpc_dep="$release-rpc" cat_dep="$release-catalog-rpc"
budget=$(helm -n "$ns" get values "$release" -a -o json | jq -r '.rpc.waitForCatalog.timeoutSeconds')
[ "$budget" -gt 0 ] || { echo "outage: rpc.waitForCatalog.timeoutSeconds is not set in the release"; exit 1; }
port=${SMOKE_OUTAGE_PORT:-18083}
api="http://127.0.0.1:$port/api"
pf=""

pf_start() {
  pf_stop
  kubectl -n "$ns" port-forward "svc/$rpc_dep" "$port:80" >/dev/null 2>&1 & pf=$!
  for _ in $(seq 1 30); do curl -fsS "$api/readyz" >/dev/null 2>&1 && return 0; sleep 1; done
  echo "outage: rpc never answered readyz through the port-forward"; return 1
}
pf_stop() { [ -n "$pf" ] && kill "$pf" 2>/dev/null && wait "$pf" 2>/dev/null || true; pf=""; }
restore() {
  pf_stop
  kubectl -n "$ns" scale "deploy/$cat_dep" --replicas=1 >/dev/null 2>&1 || true
  helm upgrade --install "$release" "$chart" -n "$ns" -f "$values" --wait --timeout 3m >/dev/null
}
trap restore EXIT

# call <path> <json> → "<http status> <x-error-code or ->" and the body in $body
call() {
  local out
  out=$(curl -sS -D - -o /tmp/outage.$$ -H 'Content-Type: application/json' "$api/$1" -d "$2" || true)
  body=$(cat /tmp/outage.$$ 2>/dev/null || true); rm -f /tmp/outage.$$
  printf '%s %s' "$(sed -n '1s/^HTTP[^ ]* \([0-9]*\).*/\1/p' <<<"$out")" "$(tr -d '\r' <<<"$out" | sed -n 's/^[Xx]-[Ee]rror-[Cc]ode: //p' | head -n 1 | grep . || echo -)"
}
want() { # <what> <got> <want>
  [ "$2" = "$3" ] || { echo "outage: $1: got '$2', want '$3' (body: ${body:-})"; exit 1; }
}
rpc_pod() { kubectl -n "$ns" get pods -l app.kubernetes.io/component=rpc --field-selector=status.phase=Running -o json \
  | jq -r '[.items[] | select(.metadata.deletionTimestamp == null)] | sort_by(.metadata.creationTimestamp) | last | .metadata.name'; }
# the wait's own log: bounded retries, a failed read is a retry, never the log. An EMPTY log is a valid answer
# (a wait that found the catalog ready logs nothing), so success is the read succeeding AND the init container having
# terminated with exit 0, not the text being non-empty.
init_log() {
  local pod=$1 out code
  for _ in $(seq 1 30); do
    code=$(kubectl -n "$ns" get pod "$pod" -o json 2>/dev/null | jq -r '.status.initContainerStatuses[0].state.terminated.exitCode // "none"') || code=none
    if [ "$code" = 0 ] && out=$(kubectl -n "$ns" logs "$pod" -c wait-for-catalog 2>/dev/null); then printf '%s' "$out"; return 0; fi
    sleep 2
  done
  return 1
}
sku="kind-outage-$(date +%s)" customer="outage-$$"
order() { call ordering.v1.OrderService/CreateOrder "{\"customer_id\":\"$customer\",\"items\":[{\"sku\":\"$sku\",\"quantity\":1}]}"; }
orders() { curl -fsS -H 'Content-Type: application/json' "$api/ordering.v1.OrderService/ListOrders" -d "{\"customer_id\":\"$customer\"}" | jq '(.orders // []) | length'; }
onhand() { curl -fsS -H 'Content-Type: application/json' "$api/inventory.v1.InventoryService/GetStock" -d "{\"sku\":\"$sku\"}" | jq -r '.stock.onHand // .stock.on_hand // 0'; }

# ── seed, with the catalog up ────────────────────────────────────────────────────────────────────────────
pf_start
r=$(call catalog.v1.CatalogService/UpsertProduct "{\"sku\":\"$sku\",\"name\":\"$sku\",\"price\":{\"amount_minor\":100,\"currency\":\"USD\"}}"); want "seed UpsertProduct" "${r%% *}" 200
r=$(call inventory.v1.InventoryService/SetStock "{\"sku\":\"$sku\",\"on_hand\":10}"); want "seed SetStock" "${r%% *}" 200
r=$(order); want "CreateOrder with the catalog up" "${r%% *}" 200
pf_stop

# ── A. the catalog is down while rpc rolls out ──────────────────────────────────────────────────────────
kubectl -n "$ns" scale "deploy/$cat_dep" --replicas=0 >/dev/null
kubectl -n "$ns" wait --for=delete pod -l app.kubernetes.io/component=catalog-rpc --timeout=90s >/dev/null 2>&1 || true
start=$SECONDS
kubectl -n "$ns" rollout restart "deploy/$rpc_dep" >/dev/null
waiting=""
for _ in $(seq 1 $((budget + 10))); do
  if [ "$(kubectl -n "$ns" get pods -l app.kubernetes.io/component=rpc -o json | jq -r 'if any(.items[]; .status.initContainerStatuses[0].state.running != null) then "yes" else "no" end')" = yes ]; then waiting=yes; break; fi
  sleep 1
done
[ "$waiting" = yes ] || { echo "outage: no rpc pod was seen waiting for the catalog (initContainer wait-for-catalog)"; kubectl -n "$ns" get pods; exit 1; }
kubectl -n "$ns" rollout status "deploy/$rpc_dep" --timeout="$((budget + 120))s" >/dev/null \
  || { echo "outage: the rpc rollout did not complete with the catalog down (a best-effort wait must not hold it)"; kubectl -n "$ns" get pods; exit 1; }
waited=$((SECONDS - start))
[ "$waited" -ge $((budget - 2)) ] || { echo "outage: the rpc came up after ${waited}s, before the ${budget}s wait ended (it did not wait for the catalog)"; exit 1; }
pod=$(rpc_pod)
log=$(init_log "$pod") || { echo "outage: the wait of $pod did not terminate with exit 0, or its log could not be read"; exit 1; }
grep -q 'starting anyway' <<<"$log" || { echo "outage: the wait did not warn that it gave up: $log"; exit 1; }
grep -qi 'kind-catalog-rpc-key\|api_key' <<<"$log" && { echo "outage: the wait's log mentions the service key"; exit 1; }
echo "outage ok (A1): with the catalog down the rpc rolled out after waiting ${waited}s of a ${budget}s budget, warned, and is Ready"

pf_start
before_orders=$(orders); before_stock=$(onhand)
r=$(order); want "CreateOrder with the catalog down" "$r" "503 catalog.unavailable"
r=$(call inventory.v1.InventoryService/SetStock "{\"sku\":\"$sku\",\"on_hand\":99}"); want "SetStock with the catalog down" "$r" "503 catalog.unavailable"
want "orders written while the catalog was down" "$(orders)" "$before_orders"
want "stock written while the catalog was down" "$(onhand)" "$before_stock"
r=$(call catalog.v1.CatalogService/GetProduct "{\"sku\":\"$sku\"}"); want "GetProduct (catalog.v1) with the catalog down" "$r" "503 catalog.unavailable"
curl -fsS "$api/readyz" >/dev/null || { echo "outage: rpc readyz fails with the catalog down: it must not depend on it"; exit 1; }
echo "outage ok (A2): CreateOrder, SetStock and catalog.v1 answer 503 catalog.unavailable and write nothing; readyz, ListOrders and GetStock are untouched"

# the catalog comes back: the SAME rpc pod works again, never restarted
restarts=$(kubectl -n "$ns" get pod "$pod" -o json | jq '[.status.containerStatuses[].restartCount] | add')
kubectl -n "$ns" scale "deploy/$cat_dep" --replicas=1 >/dev/null
kubectl -n "$ns" rollout status "deploy/$cat_dep" --timeout=180s >/dev/null
ok=""
for _ in $(seq 1 60); do r=$(order); [ "${r%% *}" = 200 ] && { ok=yes; break; }; sleep 1; done
[ "$ok" = yes ] || { echo "outage: CreateOrder did not recover after the catalog came back (last: $r)"; exit 1; }
want "the rpc pod after the recovery" "$(rpc_pod)" "$pod"
want "rpc restarts across the outage" "$(kubectl -n "$ns" get pod "$pod" -o json | jq '[.status.containerStatuses[].restartCount] | add')" "$restarts"
echo "outage ok (A3): CreateOrder works again on the same rpc pod ($pod, restarts $restarts) once the catalog is back"
pf_stop

# ── B. the catalog comes up DURING the wait ────────────────────────────────────────────────────────────
kubectl -n "$ns" scale "deploy/$cat_dep" --replicas=0 >/dev/null
kubectl -n "$ns" wait --for=delete pod -l app.kubernetes.io/component=catalog-rpc --timeout=90s >/dev/null 2>&1 || true
kubectl -n "$ns" rollout restart "deploy/$rpc_dep" >/dev/null
sleep 2
kubectl -n "$ns" scale "deploy/$cat_dep" --replicas=1 >/dev/null
kubectl -n "$ns" rollout status "deploy/$cat_dep" --timeout=180s >/dev/null
kubectl -n "$ns" rollout status "deploy/$rpc_dep" --timeout="$((budget + 120))s" >/dev/null
pod=$(rpc_pod)
log=$(init_log "$pod") || { echo "outage: the wait of $pod did not terminate with exit 0, or its log could not be read"; exit 1; }
grep -q 'starting anyway' <<<"$log" && { echo "outage: the wait gave up although the catalog came up during it (budget ${budget}s): $log"; exit 1; }
echo "outage ok (B): the catalog came up during the wait, the wait ended without a warning and the rpc started behind it"
