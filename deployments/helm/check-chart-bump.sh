#!/usr/bin/env bash
# The chart's version must go up when its templates or values change (#423): compares the chart on <base-branch>
# (fetched, then `git archive`d) with the chart in the working tree. Fails closed: a base that cannot be fetched
# or archived is a failure, never a pass.
#
#   deployments/helm/check-chart-bump.sh <base-branch> [head-chart-dir]     # CI: the PR's base branch, on pull_request
set -euo pipefail
base=${1:?usage: check-chart-bump.sh <base-branch> [head-chart-dir]}
cd "$(dirname "$0")/../.."
head=${2:-deployments/helm/x-monorepo}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git fetch -q --depth=1 origin "$base" || { echo "check-chart-bump: cannot fetch $base: the base chart is unknown, so the gate cannot pass" >&2; exit 2; }
git archive FETCH_HEAD deployments/helm/x-monorepo | tar -x -C "$tmp" || { echo "check-chart-bump: cannot read the chart on $base" >&2; exit 2; }
exec python3 deployments/helm/check-chart-bump.py "$tmp/deployments/helm/x-monorepo" "$head"
