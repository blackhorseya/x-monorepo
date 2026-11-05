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
#   e2e/image.sh --grep orders         # extra args go to playwright test
set -euo pipefail
. "$(dirname "$0")/../../../../scripts/best-effort.sh"   # best_effort: diagnostics / cleanup / wait-gone only (#462)
cd "$(dirname "$0")/.."
[ -f dist/index.html ] || { echo "image.sh: dist/ missing, run task web-build" >&2; exit 1; }

tool=${CONTAINER_TOOL:-$(command -v docker >/dev/null 2>&1 && echo docker || echo podman)}
port=${E2E_CONSOLE_PORT:-18093}
rpc_port=${E2E_RPC_PORT:-18091}
name=x-monorepo-console-e2e
oidc_name=$name-oidc
oidc_port=${E2E_CONSOLE_OIDC_PORT:-18094} 
if [ "$tool" = docker ]; then
  host_opts=(--add-host=host.docker.internal:host-gateway); upstream=http://host.docker.internal:$rpc_port
else
  host_opts=(); upstream=http://host.containers.internal:$rpc_port
fi

echo "image.sh: building localhost/x-monorepo/console:e2e with $tool"
"$tool" build -q -t localhost/x-monorepo/console:e2e "${E2E_IMAGE_CONTEXT:-.}" >/dev/null
"$tool" rm -f "$name" "$oidc_name" >/dev/null 2>&1 || true
trap '"$tool" rm -f "$name" "$oidc_name" >/dev/null 2>&1 || true' EXIT
img=localhost/x-monorepo/console:e2e
# run_console <name> <port> [-e VAR=value ...]: one container, healthy before it returns.
run_console() {
  local n=$1 p=$2; shift 2
  "$tool" run -d --name "$n" -p "127.0.0.1:$p:8080" ${host_opts[@]+"${host_opts[@]}"} -e "RPC_UPSTREAM=$upstream" "$@" "$img" >/dev/null
  for ((_try35 = 1; _try35 <= 30; _try35++)); do curl -fsS "http://127.0.0.1:$p/healthz" >/dev/null 2>&1 && return 0; sleep 0.5; done
  echo "image.sh: console container $n not healthy"; "$tool" logs "$n"; exit 1
}
run_console "$name" "$port"
# Configured for OIDC, plus an env var that is NOT on the envsubst whitelist (it must not show up anywhere).
run_console "$oidc_name" "$oidc_port" -e CONSOLE_OIDC_ISSUER=https://idp.e2e.example/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e \
  -e CONSOLE_API_KEY_LOGIN=false -e SOME_SECRET=s3cr3t-must-not-leak
# Misconfigured OIDC env must stop the container, not switch OIDC off quietly and not reach /config.json
# or the CSP: the quote-and-brace would break the JSON, a newline would pass a line-oriented grep and
# cut the CSP header short, an issuer without a client id (or the reverse) is half a login.
container_field() {   # <State field> <container>: that field of the container; a failed inspect says so and prints nothing (the caller polls or compares)
  value_or_empty "$tool" inspect -f "{{.State.$1}}" "$2"
}
expect_refused() {
  local label=$1 n=$name-refused; shift
  "$tool" rm -f "$n" >/dev/null 2>&1 || true
  "$tool" run -d --name "$n" ${host_opts[@]+"${host_opts[@]}"} -e "RPC_UPSTREAM=$upstream" "$@" "$img" >/dev/null
  local status=""
  for ((_try50 = 1; _try50 <= 40; _try50++)); do
    status=$(container_field Status "$n") || return   # a failed inspect says so and leaves status empty: the loop tries again
    [ "$status" = exited ] && break
    sleep 0.5
  done
  local code logs
  code=$(container_field ExitCode "$n") || return
  code=${code:-?}
  local lf lrc=0
  lf=$(mktemp "${TMPDIR:-/tmp}/image-logs.XXXXXX") || { echo "image.sh: cannot create a temp file for the logs of $n" >&2; exit 1; }
  "$tool" logs "$n" > "$lf" 2>&1 || lrc=$?
  if [ "$lrc" != 0 ]; then echo "image.sh: cannot read the logs of $n (exit $lrc)" >&2; fi   # the refusal text is asserted next; an empty log does not contain it
  logs=$(cat "$lf") || { echo "image.sh: cannot read the saved logs of $n" >&2; exit 1; }
  rm -f "$lf"
  "$tool" rm -f "$n" >/dev/null 2>&1 || true
  if [ "$status" != exited ] || [ "$code" = 0 ] || ! printf '%s' "$logs" | grep -q 'console-env:'; then
    echo "image.sh: expected the container to refuse '$label' (exit non-zero, say why), got status=$status exit=$code"; printf '%s\n' "$logs" | tail -5; exit 1
  fi
  echo "image.sh: refused as expected: $label"
}
expect_refused 'issuer with quote and brace' -e 'CONSOLE_OIDC_ISSUER=https://evil.example/"};x' -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'issuer with a newline' -e "CONSOLE_OIDC_ISSUER=$(printf 'https://idp.e2e.example/dex\nconnect-src *')" -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'issuer with a trailing newline' -e "CONSOLE_OIDC_ISSUER=$(printf 'https://idp.e2e.example/dex\n.')" -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'client id with a newline' -e CONSOLE_OIDC_ISSUER=https://idp.e2e.example/dex -e "CONSOLE_OIDC_CLIENT_ID=$(printf 'a\nb')"
expect_refused 'issuer with userinfo' -e CONSOLE_OIDC_ISSUER=https://user@idp.e2e.example/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'issuer with a query' -e 'CONSOLE_OIDC_ISSUER=https://idp.e2e.example/dex?x=1' -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'port 65536' -e CONSOLE_OIDC_ISSUER=https://idp.e2e.example:65536/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'port 99999' -e CONSOLE_OIDC_ISSUER=https://idp.e2e.example:99999/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'port 0' -e CONSOLE_OIDC_ISSUER=https://idp.e2e.example:0/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'IPv4 octet 256' -e CONSOLE_OIDC_ISSUER=https://10.0.0.256/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'numeric last label that is not an IPv4' -e CONSOLE_OIDC_ISSUER=https://idp.e2e.example.123/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'hex last label (.0x1)' -e CONSOLE_OIDC_ISSUER=https://idp.e2e.example.0x1/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'hex last label (.0XFF)' -e CONSOLE_OIDC_ISSUER=https://idp.e2e.example.0XFF/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'empty host label' -e CONSOLE_OIDC_ISSUER=https://idp..e2e.example/dex -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e
expect_refused 'issuer without a client id' -e CONSOLE_OIDC_ISSUER=https://idp.e2e.example/dex
expect_refused 'client id without an issuer' -e CONSOLE_OIDC_CLIENT_ID=x-console-e2e

E2E_CONSOLE_URL="http://127.0.0.1:$port" E2E_CONSOLE_OIDC_URL="http://127.0.0.1:$oidc_port" \
  pnpm exec playwright test --project image "$@"
