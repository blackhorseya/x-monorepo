#!/usr/bin/env bash
# The chromium e2e project against the real console image: builds web/apps/console/Dockerfile
# (dist/ must exist: task web-build), runs it with RPC_UPSTREAM pointing back at the host, where
# Playwright starts bin/rpc as usual, and runs the specs with E2E_CONSOLE_URL set (which also
# turns e2e/image.spec.ts on). docker or podman, Linux or a podman machine: the container reaches
# the host through host.docker.internal (host-gateway) / host.containers.internal.
#
#   task web-e2e-image                 # what CI's console-image job runs
#   e2e/image.sh --grep orders         # extra args go to playwright test
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f dist/index.html ] || { echo "image.sh: dist/ missing, run task web-build" >&2; exit 1; }

tool=${CONTAINER_TOOL:-$(command -v docker >/dev/null 2>&1 && echo docker || echo podman)}
port=${E2E_CONSOLE_PORT:-18093}
rpc_port=${E2E_RPC_PORT:-18091}
name=x-monorepo-console-e2e
oidc_name=$name-oidc bad_name=$name-bad
oidc_port=${E2E_CONSOLE_OIDC_PORT:-18094} bad_port=${E2E_CONSOLE_BAD_PORT:-18095}
if [ "$tool" = docker ]; then
  host_opts=(--add-host=host.docker.internal:host-gateway); upstream=http://host.docker.internal:$rpc_port
else
  host_opts=(); upstream=http://host.containers.internal:$rpc_port
fi

echo "image.sh: building localhost/x-monorepo/console:e2e with $tool"
"$tool" build -q -t localhost/x-monorepo/console:e2e . >/dev/null
"$tool" rm -f "$name" "$oidc_name" "$bad_name" >/dev/null 2>&1 || true
trap '"$tool" rm -f "$name" "$oidc_name" "$bad_name" >/dev/null 2>&1 || true' EXIT
img=localhost/x-monorepo/console:e2e
# run_console <name> <port> [-e VAR=value ...]: one container, healthy before it returns.
run_console() {
  local n=$1 p=$2; shift 2
  "$tool" run -d --name "$n" -p "127.0.0.1:$p:8080" ${host_opts[@]+"${host_opts[@]}"} -e "RPC_UPSTREAM=$upstream" "$@" "$img" >/dev/null
  for _ in $(seq 1 30); do curl -fsS "http://127.0.0.1:$p/healthz" >/dev/null 2>&1 && return 0; sleep 0.5; done
  echo "image.sh: console container $n not healthy"; "$tool" logs "$n"; exit 1
}
run_console "$name" "$port"
# Configured for OIDC, plus an env var that is NOT on the envsubst whitelist (it must not show up anywhere).
run_console "$oidc_name" "$oidc_port" -e CONSOLE_OIDC_ISSUER=https://idp.e2e.example/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e \
  -e CONSOLE_API_KEY_LOGIN=false -e SOME_SECRET=s3cr3t-must-not-leak
# An issuer the entrypoint must refuse (quote and brace would break the JSON; a path-less host is not the point).
run_console "$bad_name" "$bad_port" -e 'CONSOLE_OIDC_ISSUER=https://evil.example/"};x' -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e

E2E_CONSOLE_URL="http://127.0.0.1:$port" E2E_CONSOLE_OIDC_URL="http://127.0.0.1:$oidc_port" E2E_CONSOLE_BAD_URL="http://127.0.0.1:$bad_port" \
  pnpm exec playwright test --project chromium "$@"
