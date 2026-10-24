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
if [ "$tool" = docker ]; then
  host_opts=(--add-host=host.docker.internal:host-gateway); upstream=http://host.docker.internal:$rpc_port
else
  host_opts=(); upstream=http://host.containers.internal:$rpc_port
fi

echo "image.sh: building localhost/x-monorepo/console:e2e with $tool"
"$tool" build -q -t localhost/x-monorepo/console:e2e . >/dev/null
"$tool" rm -f "$name" >/dev/null 2>&1 || true
trap '"$tool" rm -f "$name" >/dev/null 2>&1 || true' EXIT
"$tool" run -d --name "$name" -p "127.0.0.1:$port:8080" ${host_opts[@]+"${host_opts[@]}"} -e "RPC_UPSTREAM=$upstream" \
  localhost/x-monorepo/console:e2e >/dev/null
for _ in $(seq 1 30); do curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.5; done
curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null || { echo "image.sh: console container not healthy"; "$tool" logs "$name"; exit 1; }

E2E_CONSOLE_URL="http://127.0.0.1:$port" pnpm exec playwright test --project chromium "$@"
