# Sourced by the k6 harnesses that start a ./bin/rpc (smoke.sh, auth-matrix.sh, bench.sh): rpc has no
# catalog of its own, so every rpc they start needs a catalog-rpc next to it, on the same database.
#   . tests/k6/catalog.sh
#   catalog_start <bin dir> <port> [log file]   # needs APP_POSTGRES_URL; exports APP_CATALOG_UPSTREAM / APP_CATALOG_API_KEY
#   catalog_stop
# It runs `catalog-rpc --require-durable` (Postgres, auth required, the one key rpc=…:admin), with
# APP_CONFIG=/dev/null so nothing of the rpc's own config (OIDC, JWT, user keys) reaches it. The key is
# made up for the test; nothing here is a credential.
catalog_pid=""
CATALOG_KEY=k6-fake-catalog-service-key-0001

catalog_start() {
  local bin=$1 port=$2 log=${3:-$1/k6-catalog.log}
  APP_CONFIG=/dev/null APP_HTTP_ADDR=":$port" APP_HTTP_DOCS=false APP_LOG_LEVEL="${APP_LOG_LEVEL:-warn}" \
    APP_AUTH_MODE=required APP_AUTH_API_KEYS="rpc=$CATALOG_KEY:admin" \
    "$bin/catalog-rpc" --require-durable > "$log" 2>&1 & catalog_pid=$!
  for _ in $(seq 1 100); do
    kill -0 "$catalog_pid" 2>/dev/null || { echo "catalog-rpc exited:"; cat "$log"; exit 1; }
    curl -fsS "http://127.0.0.1:$port/api/readyz" >/dev/null 2>&1 && break
    sleep 0.2
  done
  curl -fsS "http://127.0.0.1:$port/api/readyz" >/dev/null 2>&1 || { echo "catalog-rpc never became ready:"; cat "$log"; exit 1; }
  export APP_CATALOG_UPSTREAM="http://127.0.0.1:$port" APP_CATALOG_API_KEY=$CATALOG_KEY
}

catalog_stop() {
  [ -n "$catalog_pid" ] && kill "$catalog_pid" 2>/dev/null && wait "$catalog_pid" 2>/dev/null || true
  catalog_pid=""
}
