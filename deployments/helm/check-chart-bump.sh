#!/usr/bin/env bash
# The chart's version must go up when its templates or values change (#423): compares the chart on <base-branch>
# (fetched, then `git archive`d) with the chart in the working tree. Fails closed: a base that cannot be fetched
# or archived is a failure, never a pass.
#
#   deployments/helm/check-chart-bump.sh <base-branch> [head-chart-dir]     # CI: the PR's base branch, on pull_request
set -euo pipefail
base=${1:?usage: check-chart-bump.sh <base-branch> [head-chart-dir]}
abs_dir_of() { local p=${1%/*}; [ "$p" != "$1" ] || p=.; ( cd "$p${2:+/$2}" && pwd ); }   # the absolute directory of a path, without dirname inside $(…)
root=$(abs_dir_of "$0" ../..) || exit 1
cd "$root" || exit 1
head=${2:-deployments/helm/x-monorepo}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# --depth only when this clone is already shallow (CI): on a full clone, and above all on a repository shared by
# worktrees, `--depth` would make the whole repository shallow and break every other worktree's merge-base.
shallow=$(git rev-parse --is-shallow-repository) || { echo "check-chart-bump: cannot tell whether this clone is shallow" >&2; exit 2; }
depth=(); [ "$shallow" != true ] || depth=(--depth=1)
git fetch -q ${depth[@]+"${depth[@]}"} origin "$base" || { echo "check-chart-bump: cannot fetch $base: the base chart is unknown, so the gate cannot pass" >&2; exit 2; }
git archive FETCH_HEAD deployments/helm/x-monorepo | tar -x -C "$tmp" || { echo "check-chart-bump: cannot read the chart on $base" >&2; exit 2; }
exec python3 deployments/helm/check-chart-bump.py "$tmp/deployments/helm/x-monorepo" "$head"
