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
abs_dir_of() { local p=${1%/*}; [ "$p" != "$1" ] || p=.; ( cd "$p${2:+/$2}" && pwd ); }   # the absolute directory of a script path, no dirname inside a substitution (#472)
here=$(abs_dir_of "$0") || exit 1
shim=$here/shim lock=$here/lock.sh
[ -x "$shim/kubectl" ] && [ -f "$lock" ] || { echo "run.sh: $here is missing shim/ or lock.sh" >&2; exit 2; }

scratch=$(mktemp -d "${TMPDIR:-/tmp}/kind-run.XXXXXX") || exit 1
trap 'rm -rf "$scratch"' EXIT
trap 'exit 130' INT TERM HUP
mkdir -p "$scratch/home" "$scratch/tmp"
: > "$scratch/home/.kind-runner"          # the marker kind/*.sh look for (#444)
kc_dir=${kc%/*} kc_base=${kc##*/}       # kc is absolute (above): the directory and the file name, without dirname / basename in a substitution
kc_name=${kc_base#kubeconfig-}          # == sed 's/^kubeconfig-//'

# PATH: the shims, then the tools themselves, then the directory of each tool and the system directories. The directories are the ones the
# CALLER's PATH finds the tools in; the PATH built from them is then searched ONCE for each tool, and that answer is linked into
# $scratch/bin, ahead of everything else. The link is what the command runs (the shim's real kubectl, a script's `kind`) and what the entry
# check below runs, so the check and the command use the same binaries by construction, whatever else is on the caller's PATH.
mkdir -p "$scratch/bin"
tools="kubectl helm kind docker podman k6 jq psql python3 curl git go perl base64 xargs"
dirs=""
for t in $tools; do
  p=""
  # a tool the caller does not have is simply not added to the PATH; the script that needs it fails on its own
  if type -P "$t" >/dev/null 2>&1; then p=$(type -P "$t") || exit 1; fi
  case $p in /*) d=${p%/*}; case ":$dirs:" in *":$d:"*) ;; *) dirs=$dirs:$d ;; esac ;; esac
done
rest=${dirs#:}:/usr/bin:/bin:/usr/sbin:/sbin
for t in $tools; do
  p=$(PATH=$rest; type -P "$t") || continue   # not found in any of those directories: no link
  case $p in /*) ln -s "$p" "$scratch/bin/$t" || exit 1 ;; esac
done
path=$shim:$scratch/bin:$rest

# The entry check, the only check of where this lands: before a kubectl / helm / script touches the cluster, the kubeconfig it was given
# must be the kind cluster it is named after (kubeconfig-<name> -> kind cluster <name>): one cluster, one context, and the API server
# `kind get kubeconfig --name <name>` reports now. A context NAME proves nothing (an alias can carry any name), the server address does.
# Anything else, or a cluster kind does not have, stops here (fail closed). A file that does not exist yet passes (kubectl fails on it
# anyway), and `kind` commands are not checked: they act on the container runtime by name, and kind create / delete only add or remove
# their own entry in the file they are given. It runs the same kind and kubectl the command will (the links in $scratch/bin, see PATH below).
if [ "$1" != kind ] && [ -e "$kc" ]; then
  "$scratch/bin/kind" get kubeconfig --name "$kc_name" > "$scratch/want.yaml" 2>/dev/null \
    || { echo "run.sh: refusing to run: kind has no cluster '$kc_name' (the kubeconfig $kc is named after it)" >&2; exit 1; }
  have=$("$scratch/bin/kubectl" config view --kubeconfig "$kc" -o 'jsonpath={.clusters[*].cluster.server} {.contexts[*].name}') \
    || { echo "run.sh: refusing to run: cannot read $kc" >&2; exit 1; }
  want=$("$scratch/bin/kubectl" config view --kubeconfig "$scratch/want.yaml" -o 'jsonpath={.clusters[*].cluster.server} {.contexts[*].name}') \
    || { echo "run.sh: refusing to run: cannot read what kind reports for '$kc_name'" >&2; exit 1; }
  [ -n "$want" ] && [ "$have" = "$want" ] \
    || { echo "run.sh: refusing to run: $kc is not the kubeconfig of kind cluster '$kc_name' (got '$have', kind says '$want')" >&2; exit 1; }
fi
helm_home=$kc_dir/helm-$kc_name
mkdir -p "$helm_home/config" "$helm_home/cache" "$helm_home/data"

# the only names that come through from the caller (nothing that names a kubeconfig, a context, a cluster)
pass=(LANG LC_ALL LC_CTYPE TERM TZ HTTP_PROXY HTTPS_PROXY NO_PROXY http_proxy https_proxy no_proxy SSL_CERT_FILE
      DOCKER_HOST DOCKER_CONTEXT CONTAINER_HOST CONTAINER_CONNECTION CONTAINERS_CONF CONTAINERS_STORAGE_CONF XDG_RUNTIME_DIR
      KIND_EXPERIMENTAL_PROVIDER KIND_EXPERIMENTAL_DOCKER_NETWORK
      RUNNER_TEMP NEGATIVE_SLEEP SMOKE_PORT SMOKE_CONSOLE_PORT SMOKE_CATALOG_PORT)
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
