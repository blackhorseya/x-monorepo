#!/usr/bin/env bash
# Decides which ci.yml jobs a change needs, from the files it touches. Prints `run_<job>=true|false`
# lines (ci.yml's affected job copies them to GITHUB_OUTPUT; every other job has
# `if: needs.affected.outputs.run_<job> == 'true'`).
#
#   scripts/ci-changes.sh [base] [head]   # default origin/main...HEAD (the PR's diff)
#   CI_ALL=1 scripts/ci-changes.sh        # everything true: main (the release gate) and dispatch
#
# Areas → jobs. A file that no area claims (README, docs/, the compose file, .claude, e2e specs of
# other areas…) runs nothing but what it belongs to. ci.yml,
# .github/actions, Taskfile.yml and scripts/ are inputs to every job, so touching them runs all.
# The few files outside those that scripts/test.sh reads also run scripts-test (scripts_src below).
#
# The heavy jobs are narrower than "any Go file changed" (#328); when a path is not named below, the
# job keeps running (a skipped job is only right when its inputs are known not to have changed):
#   *_test.go only      the Go checks (gen-freshness, lint, work-sync, coverage) and the test matrix;
#                       no build, no integration job. A test helper not named *_test.go is product code.
#   console-image       web, proto (the TS client is generated from it), image files; not plain Go.
#   release-snapshot    .goreleaser.yaml, a Dockerfile, .dockerignore, release.yml, go.mod / go.sum / go.work.
#   helm-kind           product Go, web, helm, image files.
#   k6 (the smoke)      tests/k6, pkgs/platform/server + config (rate limit, timeout), apps/rpc, apps/worker,
#                       configs, go.mod / go.sum / go.work. Business flows are helm-kind's.
#   k6-auth             tests/k6, pkgs/platform/server + config, pkgs/identity, apps/rpc, configs.
#   k6-wire             tests/k6 and the module graph, no Go code: it guards against the API key leaking (#209) with k6 + python3 against
#                       wire_server.py and a stub rpc, and builds nothing. go.work is read by `task` itself (Taskfile.yml evaluates
#                       `go work edit -json go.work` on every run) and by the setup action (go-version-file); go.mod / go.sum / go.work.sum
#                       are the module graph the setup action's `go mod download` resolves when the module cache is not an exact hit.
#                       scripts/, Taskfile.yml and .github/ run it through ci.
set -euo pipefail
self_dir=${0%/*}; [ "$self_dir" != "$0" ] || self_dir=.   # dirname without a $(…)
cd "$self_dir/.." || exit 1

if [ -n "${CI_ALL:-}" ]; then
  files=ALL
else
  base=${1:-origin/main} head=${2:-HEAD}
  # --no-renames: a rename is a delete plus an add, both paths count (helper.go -> helper_test.go removes a product file).
  files=$(git diff --no-renames --name-only "$base...$head") || exit $?
fi

# has <glob>...: does any changed file match one of the globs (bash pattern, anchored at the root)?
has() {
  [ "$files" = ALL ] && return 0
  local f p
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    for p in "$@"; do
      # shellcheck disable=SC2254
      case "$f" in $p) return 0 ;; esac
    done
  done <<<"$files"
  return 1
}

# has_prod <glob>...: like has, but a *_test.go never counts (a PR that only touches tests changes no binary).
has_prod() {
  [ "$files" = ALL ] && return 0
  local f p
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    case "$f" in *_test.go) continue ;; esac
    for p in "$@"; do
      # shellcheck disable=SC2254
      case "$f" in $p) return 0 ;; esac
    done
  done <<<"$files"
  return 1
}

ci=false;     has '.github/workflows/ci.yml' '.github/actions/*' 'Taskfile.yml' 'scripts/*' && ci=true
actions=false; has '.github/*' && actions=true
proto=false;  has 'protos/*' 'buf.yaml' 'buf.gen.yaml' 'buf.lock' 'buf.gen.ts.yaml' && proto=true
# go: anything the Go checks (gen-freshness, lint, work-sync, coverage) read, test files included.
go=false;     has '*.go' 'go.work' 'go.work.sum' '*/go.mod' '*/go.sum' '.golangci.yml' 'configs/*' && go=true
[ $proto = true ] && go=true   # generated Go is committed; a proto change is a Go change
# go_prod: Go that reaches a binary. A *_test.go alone is not (no build, no integration job); a test helper
# in a plain .go file is. Fed to every job that builds or runs the binaries.
go_prod=false; has_prod '*.go' 'go.work' 'go.work.sum' '*/go.mod' '*/go.sum' '.golangci.yml' 'configs/*' && go_prod=true
[ $proto = true ] && go_prod=true
gomod=false;  has 'go.work' 'go.work.sum' '*/go.mod' '*/go.sum' && gomod=true   # the dependency graph of every binary
web=false;    has 'web/*' && web=true
newbc=false;  has 'tools/newbc/*' && newbc=true   # the generator and its templates: gen-freshness runs task new-bc-check
helm=false;   has 'deployments/helm/*' && helm=true
# The dashboards and alert rules in deployments/docker/grafana belong to the helm job: a change to them runs that job — and only
# that: they render nothing the cluster installs.
grafana=false; has 'deployments/docker/grafana/*' && grafana=true
release=false; has '.github/workflows/release.yml' && release=true   # pins cosign-release: the smoke + Rekor canary must run on a bump (#215)
images=false; has 'deployments/docker/Dockerfile*' 'web/apps/console/Dockerfile' 'web/apps/console/deploy/*' '.goreleaser.yaml' '.dockerignore' && images=true
# goreleaser builds the Go images from deployments/docker/Dockerfile*; the console image is built by release.yml
# with buildx (.goreleaser.yaml says so), so its Dockerfile and nginx config are not a release-snapshot input.
snapshot_src=false; has 'deployments/docker/Dockerfile*' '.goreleaser.yaml' '.dockerignore' && snapshot_src=true
k6=false;     has 'tests/k6/*' && k6=true
# What the k6 jobs run is a ./bin/rpc (+ ./bin/worker): the limits and timeout interceptors live in pkgs/platform/server
# (limits.go, server.go) and are configured from pkgs/platform/config; auth in the same server package, pkgs/identity
# and apps/rpc. Test files are no input (has_prod).
k6_src=false;   { has_prod 'pkgs/platform/server/*' 'pkgs/platform/config/*' 'apps/rpc/*' 'apps/worker/*' || has 'configs/*'; } && k6_src=true
auth_src=false; { has_prod 'pkgs/platform/server/*' 'pkgs/platform/config/*' 'pkgs/identity/*' 'apps/rpc/*' || has 'configs/*'; } && auth_src=true
# The oidc-dex Playwright project signs in through the compose Dex (#274): its config or the service is a console-e2e input.
dex=false;    has 'deployments/docker/dex/*' 'deployments/docker/docker-compose.yaml' && dex=true
# Inputs of scripts/test.sh outside scripts/, ci.yml, Taskfile.yml and .github/actions (those already run everything): .goreleaser.yaml (the
# release image list cases), go.work (the MODULES
# cases copy it), the console image's 18-console-env.envsh (its OIDC gate cases). Add a path here when a kept test.sh case starts reading a new one.
scripts_src=false; has '.goreleaser.yaml' 'go.work' 'web/apps/console/deploy/18-console-env.envsh' && scripts_src=true


any() { for v in "$@"; do [ "$v" = true ] && return 0; done; return 1; }
out() { if any "${@:2}"; then echo "run_$1=true"; else echo "run_$1=false"; fi; }

out gen_freshness    "$go" "$web" "$ci" "$newbc"
out lint             "$go" "$ci"
out work_sync        "$go" "$ci"
out coverage         "$go" "$ci"
out build            "$go_prod" "$ci"
out release_snapshot "$gomod" "$snapshot_src" "$release" "$ci"
out actions_resolve  "$actions" "$ci"
out web              "$web" "$proto" "$ci"
out console_e2e      "$go_prod" "$web" "$dex" "$ci"
out console_image    "$web" "$proto" "$images" "$ci"
out helm             "$helm" "$grafana" "$ci"
out scripts_test     "$ci" "$scripts_src"
out helm_kind        "$go_prod" "$web" "$helm" "$images" "$ci"
out k6               "$k6" "$k6_src" "$gomod" "$ci"
out k6_auth          "$k6" "$auth_src" "$ci"
out k6_wire          "$gomod" "$k6" "$ci"
