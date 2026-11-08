#!/usr/bin/env bash
# Is this PR's green CI about the tree that merging would produce? Run before every merge:
#
#   scripts/merge-ready.sh <pr>      # task merge-ready -- <pr>
#
# A pull_request run checks out refs/pull/N/merge: the PR head merged into the base branch *as it
# was when the run started*. Once main moves (another PR merged), that green says nothing about
# main + this PR — #187 changed NewCreateOrder's signature while #188's new test still used the
# old one; both were green, together they do not build. This script reads the latest ci.yml
# pull_request run for the PR's current head, finds its merge commit, and compares the merge
# commit's base parent with main *now*:
#   fresh  base parent == main HEAD and head parent == PR head   → the green is about this merge
#   STALE  anything else                                         → not proven; rebase / update the branch, rerun CI
# Also needs the run finished and every check of the PR pass / skipping (no pending, fail, cancel).
#
# Exit: 0 ready · 1 checks missing, pending or failing · 2 stale / unproven · 3 usage.
set -euo pipefail
_lib=${BASH_SOURCE[0]%/*}; [ "$_lib" != "${BASH_SOURCE[0]}" ] || _lib=.   # this script's own directory, whatever the cwd (parameter expansion only)
. "$_lib/best-effort.sh" || { echo "merge-ready: cannot load $_lib/best-effort.sh (value_of)" >&2; exit 2; }   # a missing library stops the script (#470)
usage() { echo "usage: $0 <pr>" >&2; exit 3; }
[ $# -eq 1 ] || usage
pr=$1
case $pr in ''|*[!0-9]*) usage ;; esac

repo=${MERGE_READY_REPO:-}
if [ -z "$repo" ]; then repo=$(gh repo view --json nameWithOwner -q .nameWithOwner) || exit 1; fi
# `set -- $(gh …)` would drop gh's exit status (#462): read first, split after
view=$(gh pr view "$pr" --repo "$repo" --json headRefOid,baseRefName -q '.headRefOid + " " + .baseRefName') || exit $?
# shellcheck disable=SC2086
set -- $view
head=${1:?gh pr view printed no head} base=${2:?gh pr view printed no base}
main=$(gh api "repos/$repo/git/ref/heads/$base" -q .object.sha) || exit $?

echo "PR #$pr → $base"
echo "  main   $main   (live $base HEAD)"
echo "  head   $head   (PR head)"

# ── checks: complete, none pending / failing ──
# gh pr checks exits 1 when a check failed and 8 when checks are pending, and prints the list either way; any other
# status is gh itself failing, and an unreadable list is not "no checks" (#462)
capture buckets rc gh pr checks "$pr" --repo "$repo" --json bucket -q '.[].bucket' 2>/dev/null || exit 1
case $rc in
  0|1|8) ;;
  *) echo "NOT READY: gh pr checks failed (exit $rc) — the checks of PR #$pr could not be read" >&2; exit 1 ;;
esac
total=$(value_of -ok 1 grep -c . <<<"$buckets") || exit 1
bad=$(value_of -ok 1 grep -cE '^(fail|cancel)$' <<<"$buckets") || exit 1
pending=$(value_of -ok 1 grep -cE '^pending$' <<<"$buckets") || exit 1

runinfo=$(gh run list --workflow ci.yml --event pull_request --commit "$head" --repo "$repo" --limit 1 \
  --json databaseId,status,conclusion -q '.[0] // empty | "\(.databaseId) \(.status) \(.conclusion)"') || exit $?
# shellcheck disable=SC2086
set -- $runinfo
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
log=$(gh run view "$run" --repo "$repo" --log) || exit $?
merge=$(printf '%s\n' "$log" | sed -n "s|.*+\([0-9a-f]\{40\}\):refs/remotes/pull/$pr/merge.*|\1|p" | head -1) || exit $?
if [ -z "$merge" ]; then
  echo "  STALE: run $run did not check out refs/pull/$pr/merge — nothing proves which main it merged into"
  fresh=false
else
  parents=$(gh api "repos/$repo/commits/$merge" -q '.parents[].sha') || exit $?
  # shellcheck disable=SC2086
  set -- $parents
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

# ── stale: the green is not about main + this head — never ready ──
echo "NOT READY (stale): rebase or update the branch onto main $main, then rerun CI and run this again"
exit 2
