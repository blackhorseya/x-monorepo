#!/usr/bin/env bash
# Decides which ci.yml jobs a change needs, from the files it touches. Prints `run_<job>=true|false`
# lines (ci.yml's affected job copies them to GITHUB_OUTPUT; every other job has
# `if: needs.affected.outputs.run_<job> == 'true'`).
#
#   scripts/ci-changes.sh [base] [head]   # default origin/main...HEAD (the PR's diff)
#   CI_ALL=1 scripts/ci-changes.sh        # everything true: main (the release gate) and dispatch
#
# Areas → jobs. A file that no area claims (README, docs/, Grafana dashboards and alerts, the
# compose file, .claude, e2e specs of other areas…) runs nothing but what it belongs to. ci.yml,
# .github/actions, Taskfile.yml and scripts/ are inputs to every job, so touching them runs all.
# Scripts outside scripts/ that scripts/test.sh drives (coredns-dex.sh) also run scripts-test.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -n "${CI_ALL:-}" ]; then
  files=ALL
else
  base=${1:-origin/main} head=${2:-HEAD}
  files=$(git diff --name-only "$base...$head")
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

ci=false;     has '.github/workflows/ci.yml' '.github/actions/*' 'Taskfile.yml' 'scripts/*' && ci=true
actions=false; has '.github/*' && actions=true
proto=false;  has 'protos/*' 'buf.yaml' 'buf.gen.yaml' 'buf.lock' 'buf.gen.ts.yaml' && proto=true
go=false;     has '*.go' 'go.work' 'go.work.sum' '*/go.mod' '*/go.sum' '.golangci.yml' 'configs/*' && go=true
[ $proto = true ] && go=true   # generated Go is committed; a proto change is a Go change
web=false;    has 'web/*' && web=true
helm=false;   has 'deployments/helm/*' && helm=true
release=false; has '.github/workflows/release.yml' && release=true   # pins cosign-release: the smoke + Rekor canary must run on a bump (#215)
images=false; has 'deployments/docker/Dockerfile*' 'web/apps/console/Dockerfile' 'web/apps/console/deploy/*' '.goreleaser.yaml' '.dockerignore' && images=true
k6=false;     has 'tests/k6/*' && k6=true
# scripts/test.sh runs this script itself (its coredns: cases): a change to it must run scripts-test (#287).
scripts_src=false; has 'deployments/helm/kind/coredns-dex.sh' && scripts_src=true

any() { for v in "$@"; do [ "$v" = true ] && return 0; done; return 1; }
out() { if any "${@:2}"; then echo "run_$1=true"; else echo "run_$1=false"; fi; }

out gen_freshness    "$go" "$web" "$ci"
out lint             "$go" "$ci"
out work_sync        "$go" "$ci"
out coverage         "$go" "$ci"
out build            "$go" "$ci"
out breaking         "$proto" "$ci"
out release_snapshot "$go" "$images" "$release" "$ci"
out actions_resolve  "$actions" "$ci"
out web              "$web" "$proto" "$ci"
out console_e2e      "$go" "$web" "$ci"
out console_image    "$go" "$web" "$images" "$ci"
out helm             "$helm" "$ci"
out scripts_test     "$ci" "$scripts_src"
out helm_kind        "$go" "$web" "$helm" "$images" "$ci"
out k6               "$go" "$k6" "$ci"
