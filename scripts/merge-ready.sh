#!/usr/bin/env bash
# Is this PR's green CI about the tree that merging would produce? Run before every merge:
#
#   scripts/merge-ready.sh <pr> [--revalidate]      # task merge-ready -- <pr>
#
# A pull_request run checks out refs/pull/N/merge: the PR head merged into the base branch *as it
# was when the run started*. Once main moves (another PR merged), that green says nothing about
# main + this PR — #187 changed NewCreateOrder's signature while #188's new test still used the
# old one; both were green, together they do not build. This script reads the latest ci.yml
# pull_request run for the PR's current head, finds its merge commit, and compares the merge
# commit's base parent with main *now*:
#   fresh  base parent == main HEAD and head parent == PR head   → the green is about this merge
#   STALE  anything else                                         → not proven; revalidate first
# Also needs the run finished and every check of the PR pass / skipping (no pending, fail, cancel).
#
# --revalidate (stale only): dispatch .github/workflows/merge-tree.yml for (pr, head, main), which
# builds main + head with scripts/merge-tree-check.sh. `gh run rerun` is NOT used: a rerun reuses
# the original GITHUB_SHA / merge ref, so it would re-run the same stale tree. The PR head never
# changes (no "update branch"), so a reviewed head stays the reviewed head. A successful
# merge-tree run for exactly this pr / head / main counts as the composite check on the next call.
#
# Exit: 0 ready · 1 checks missing, pending or failing · 2 stale / unproven · 3 usage.
set -euo pipefail
usage() { echo "usage: $0 <pr> [--revalidate]" >&2; exit 3; }
[ $# -ge 1 ] && [ $# -le 2 ] || usage
pr=$1 revalidate=false
case "${2:-}" in "") ;; --revalidate) revalidate=true ;; *) usage ;; esac
case $pr in ''|*[!0-9]*) usage ;; esac

repo=${MERGE_READY_REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}
# shellcheck disable=SC2046
set -- $(gh pr view "$pr" --repo "$repo" --json headRefOid,baseRefName -q '.headRefOid + " " + .baseRefName')
head=$1 base=$2
main=$(gh api "repos/$repo/git/ref/heads/$base" -q .object.sha)

echo "PR #$pr → $base"
echo "  main   $main   (live $base HEAD)"
echo "  head   $head   (PR head)"

# ── checks: complete, none pending / failing ──
buckets=$(gh pr checks "$pr" --repo "$repo" --json bucket -q '.[].bucket' 2>/dev/null || true)
total=$(printf '%s\n' "$buckets" | grep -c . || true)
bad=$(printf '%s\n' "$buckets" | grep -cE '^(fail|cancel)$' || true)
pending=$(printf '%s\n' "$buckets" | grep -cE '^pending$' || true)

# shellcheck disable=SC2046
set -- $(gh run list --workflow ci.yml --event pull_request --commit "$head" --repo "$repo" --limit 1 \
  --json databaseId,status,conclusion -q '.[0] // empty | "\(.databaseId) \(.status) \(.conclusion)"')
run=${1:-} status=${2:-} conclusion=${3:-}

notready=false
if [ -z "$run" ]; then
  echo "  NOT READY: no ci.yml pull_request run for head $head"; notready=true
elif [ "$status" != completed ]; then
  echo "  NOT READY: ci run $run is $status"; notready=true
elif [ "$conclusion" != success ]; then
  echo "  NOT READY: ci run $run concluded $conclusion"; notready=true
fi
if [ "$total" = 0 ]; then
  echo "  NOT READY: the PR has no checks"; notready=true
else
  echo "  checks $total checks: $bad failing, $pending pending"
  if [ "$bad" != 0 ] || [ "$pending" != 0 ]; then notready=true; fi
fi
if $notready; then exit 1; fi

# ── composite tree: which main did the run merge into? ──
log=$(gh run view "$run" --repo "$repo" --log)
merge=$(printf '%s\n' "$log" | sed -n "s|.*+\([0-9a-f]\{40\}\):refs/remotes/pull/$pr/merge.*|\1|p" | head -1)
if [ -z "$merge" ]; then
  echo "  STALE: run $run did not check out refs/pull/$pr/merge — nothing proves which main it merged into"
  fresh=false
else
  # shellcheck disable=SC2046
  set -- $(gh api "repos/$repo/commits/$merge" -q '.parents[].sha')
  p_base=${1:-} p_head=${2:-}
  echo "  merge  $merge   (ci run $run, refs/pull/$pr/merge)"
  echo "  base   $p_base   (merge parent 1: main when the run started)"
  echo "  check  head $p_head   (merge parent 2: the head the run built)"
  if [ "$p_base" = "$main" ] && [ "$p_head" = "$head" ]; then
    fresh=true
  else
    fresh=false
    [ "$p_base" = "$main" ] || echo "  STALE: the run merged into $p_base, main is now $main"
    [ "$p_head" = "$head" ] || echo "  STALE: the run built head $p_head, the PR head is $head"
  fi
fi

if $fresh; then
  echo "READY: $total checks complete and green on main $main + head $head"
  exit 0
fi

# ── stale: a merge-tree run for exactly (pr, head, main) is the other way to be proven ──
tree=$(gh run list --workflow merge-tree.yml --event workflow_dispatch --repo "$repo" --limit 30 \
  --json databaseId,status,conclusion,displayTitle \
  -q '.[] | "\(.databaseId) \(.status) \(.conclusion) \(.displayTitle)"' 2>/dev/null \
  | grep -F " completed success merge-tree pr=$pr head=$head base=$main" | head -1 || true)
if [ -n "$tree" ]; then
  echo "READY: merge-tree run ${tree%% *} built main $main + head $head and passed ($total checks green on the PR)"
  exit 0
fi
echo "NOT READY (stale): re-verify main $main + head $head before merging"
if $revalidate; then
  gh workflow run merge-tree.yml --repo "$repo" -f "pr=$pr" -f "head=$head" -f "base=$main"
  echo "  dispatched merge-tree.yml (pr=$pr head=$head base=$main); run this again when it finishes"
else
  echo "  scripts/merge-ready.sh $pr --revalidate dispatches the check without touching the PR head"
fi
exit 2
