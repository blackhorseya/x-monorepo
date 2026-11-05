#!/usr/bin/env bash
# The one door to a kind cluster: every kubectl / helm / kind command and every kind/*.sh of the kind tasks runs through here.
#
#   run.sh <kubeconfig file> <command> [args...]       e.g.  run.sh bin/kind/kubeconfig-foo kubectl -n ns get pods
#
# On this machine ~/.kube/config can point at a production cluster. The guarantee is not "the command line says the right thing"
# (a gate that reads shell text is always one shell form short: PR #435 r1-r4) but "the caller's kubeconfig is not in reach":
#   - `env -i`: the caller's environment is dropped. Only the names below come through; none is a KUBECONFIG equivalent.
#   - HOME is an empty scratch directory (removed on exit): no ~/.kube/config, no ~/.kube/cache, nothing to fall back to.
#     helm's config / cache / data live next to the kubeconfig (bin/kind/helm-<cluster>/), so `helm repo add` survives between commands.
#   - KUBECONFIG and KIND_KUBECONFIG are the given file; PATH is the shims first, then the directories of the tools the scripts use.
#   - BASH_ENV=lock.sh makes KUBECONFIG KIND_KUBECONFIG PATH HOME read-only in every bash the command starts, so a script that does
#     `unset KUBECONFIG`, `read -r KUBECONFIG`, `export HOME=...` cannot change them (the shell says "readonly variable").
#   - the shims add `--kubeconfig <file>` to every kubectl / helm found through PATH; the Taskfile's commands carry the flag too.
# What is NOT covered: see CLAUDE.md "kind target" (a script that names the caller's real absolute path, or looks up another
# user's home with ~user / getent, is outside what any environment can stop; a non-bash child can still rewrite its own variables).
set -euo pipefail

[ "$#" -ge 2 ] || { echo "usage: run.sh <kubeconfig file> <command> [args...]" >&2; exit 2; }
kc=$1; shift
case $kc in /*) ;; *) kc=$PWD/$kc ;; esac
here=$(cd "$(dirname "$0")" && pwd)
shim=$here/shim lock=$here/lock.sh
[ -x "$shim/kubectl" ] && [ -f "$lock" ] || { echo "run.sh: $here is missing shim/ or lock.sh" >&2; exit 2; }

scratch=$(mktemp -d "${TMPDIR:-/tmp}/kind-run.XXXXXX")
trap 'rm -rf "$scratch"' EXIT
trap 'exit 130' INT TERM HUP
mkdir -p "$scratch/home" "$scratch/tmp"
: > "$scratch/home/.kind-runner"          # the marker kind/*.sh look for (#444)
helm_home=$(dirname "$kc")/helm-$(basename "$kc" | sed 's/^kubeconfig-//')
mkdir -p "$helm_home/config" "$helm_home/cache" "$helm_home/data"

# PATH: the shims, then the directory of each tool the scripts use as the CALLER's PATH finds it, then the system directories
path=$shim
for t in kubectl helm kind docker podman k6 jq psql python3 curl git go perl base64 xargs; do
  p=$(command -v "$t" 2>/dev/null || true)  # exit-ok: a tool the caller does not have is simply not added to the PATH; the script that needs it fails on its own
  case $p in /*) d=$(dirname "$p"); case ":$path:" in *":$d:"*) ;; *) path=$path:$d ;; esac ;; esac
done
path=$path:/usr/bin:/bin:/usr/sbin:/sbin

# the only names that come through from the caller (nothing that names a kubeconfig, a context, a cluster)
pass=(LANG LC_ALL LC_CTYPE TERM TZ HTTP_PROXY HTTPS_PROXY NO_PROXY http_proxy https_proxy no_proxy SSL_CERT_FILE
      DOCKER_HOST DOCKER_CONTEXT CONTAINER_HOST CONTAINER_CONNECTION CONTAINERS_CONF CONTAINERS_STORAGE_CONF XDG_RUNTIME_DIR
      KIND_EXPERIMENTAL_PROVIDER KIND_EXPERIMENTAL_DOCKER_NETWORK
      RUNNER_TEMP PAYMENT_UPGRADE_FROM_TAG PAYMENT_UPGRADE_FROM_IMAGE NEGATIVE_SLEEP SMOKE_PORT SMOKE_CONSOLE_PORT SMOKE_CATALOG_PORT)
envs=()
for n in "${pass[@]}"; do
  if [ -n "${!n:-}" ]; then envs+=("$n=${!n}"); fi
done
# container engines keep their connections and contexts under the caller's HOME: point them at it (these hold no kubeconfig)
real_home=${HOME:-}
if [ -n "$real_home" ]; then
  envs+=("XDG_CONFIG_HOME=${XDG_CONFIG_HOME:-$real_home/.config}" "XDG_DATA_HOME=${XDG_DATA_HOME:-$real_home/.local/share}")
  if [ -n "${DOCKER_CONFIG:-}" ]; then envs+=("DOCKER_CONFIG=$DOCKER_CONFIG"); elif [ -d "$real_home/.docker" ]; then envs+=("DOCKER_CONFIG=$real_home/.docker"); fi
fi

set +e
env -i "${envs[@]}" \
  HOME="$scratch/home" TMPDIR="$scratch/tmp" PATH="$path" \
  KUBECONFIG="$kc" KIND_KUBECONFIG="$kc" KIND_RUNNER_HOME="$scratch/home" BASH_ENV="$lock" \
  HELM_CONFIG_HOME="$helm_home/config" HELM_CACHE_HOME="$helm_home/cache" HELM_DATA_HOME="$helm_home/data" \
  "$@"
rc=$?
exit "$rc"
