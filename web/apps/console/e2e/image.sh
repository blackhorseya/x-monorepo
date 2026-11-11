#!/usr/bin/env bash
# The "image" e2e project against the real console image: builds web/apps/console/Dockerfile
# (dist/ must exist: task web-build), runs it with RPC_UPSTREAM pointing back at the host, where
# Playwright starts bin/rpc as usual, and runs the specs with E2E_CONSOLE_URL set (which also
# turns e2e/image.spec.ts on). Only the specs that need the image run here (e2e/image-specs.ts: image.spec.ts
# and a smoke of each route family); the others run on vite preview in console-e2e (#449).
# E2E_IMAGE_CONTEXT: build from another context directory (e2e/image-mutants.sh builds a broken nginx config that way).
# docker or podman, Linux or a podman machine: the container reaches
# the host through host.docker.internal (host-gateway) / host.containers.internal.
#
#   task web-e2e-image                 # what CI's console-image job runs
#   e2e/image.sh --grep deep            # extra args go to playwright test
set -euo pipefail
abs_dir_of() { local p=${1%/*}; [ "$p" != "$1" ] || p=.; ( cd "$p${2:+/$2}" && pwd ); }   # the absolute directory of a path, without dirname inside $(…)
app_dir=$(abs_dir_of "$0" ..) || exit 1
cd "$app_dir" || exit 1
[ -f dist/index.html ] || { echo "image.sh: dist/ missing, run task web-build" >&2; exit 1; }

if [ -n "${CONTAINER_TOOL:-}" ]; then tool=$CONTAINER_TOOL; elif command -v docker >/dev/null 2>&1; then tool=docker; else tool=podman; fi
port=${E2E_CONSOLE_PORT:-18093}
rpc_port=${E2E_RPC_PORT:-18091}
name=x-monorepo-console-e2e
if [ "$tool" = docker ]; then
  host_opts=(--add-host=host.docker.internal:host-gateway); upstream=http://host.docker.internal:$rpc_port
else
  host_opts=(); upstream=http://host.containers.internal:$rpc_port
fi

echo "image.sh: building localhost/x-monorepo/console:e2e with $tool"
"$tool" build -q -t localhost/x-monorepo/console:e2e "${E2E_IMAGE_CONTEXT:-.}" >/dev/null
"$tool" rm -f "$name" >/dev/null 2>&1 || true
trap '"$tool" rm -f "$name" >/dev/null 2>&1 || true' EXIT
img=localhost/x-monorepo/console:e2e
# run_console <name> <port> [-e VAR=value ...]: one container, healthy before it returns.
run_console() {
  local n=$1 p=$2; shift 2
  "$tool" run -d --name "$n" -p "127.0.0.1:$p:8080" ${host_opts[@]+"${host_opts[@]}"} -e "RPC_UPSTREAM=$upstream" "$@" "$img" >/dev/null
  for ((_try35 = 1; _try35 <= 30; _try35++)); do curl -fsS "http://127.0.0.1:$p/healthz" >/dev/null 2>&1 && return 0; sleep 0.5; done
  echo "image.sh: console container $n not healthy"; "$tool" logs "$n"; exit 1
}
run_console "$name" "$port"
E2E_CONSOLE_URL="http://127.0.0.1:$port" \
  pnpm exec playwright test --project image "$@"
