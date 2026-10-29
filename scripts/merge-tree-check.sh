#!/usr/bin/env bash
# Does the *merged* tree of two commits compile? The check a green PR run cannot give once main
# has moved: each of #187 (NewCreateOrder gains a parameter) and #188 (a new test calls the old
# signature) built fine alone and merged without a text conflict, yet main + both does not build.
#
#   scripts/merge-tree-check.sh <base> <head>     # base = origin/main, head = the PR head
#
# Merges in memory (git merge-tree --write-tree, git >= 2.38), exports that tree to a temp dir —
# no worktree, no branch, nothing in the repo changes, and neither ref moves — and runs
# `go vet ./...` (compiles the tests too) in every go.work module (or the repo root without one).
# MERGE_TREE_CMD replaces the vet step: it runs in each module dir.
# Exit: 0 builds · 1 does not build · 2 text conflict · 3 usage.
set -euo pipefail
[ $# -eq 2 ] || { echo "usage: $0 <base> <head>" >&2; exit 3; }
base=$1 head=$2
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if ! merged=$(git merge-tree --write-tree "$base" "$head" 2>&1); then
  echo "merge-tree-check: $base + $head conflict in git itself (not a compile question):" >&2
  printf '%s\n' "$merged" >&2
  exit 2
fi
tree=${merged%%$'\n'*}
git archive "$tree" | tar -x -C "$tmp"
echo "merge-tree-check: $base + $head → tree $tree"

if [ -f "$tmp/go.work" ]; then
  dirs=$(cd "$tmp" && go list -m -f '{{.Dir}}')
else
  dirs=$tmp
fi
status=0
for dir in $dirs; do
  rel=${dir#"$tmp"}; rel=${rel#/}; rel=${rel:-.}
  if (cd "$dir" && eval "${MERGE_TREE_CMD:-go vet ./...}") >"$tmp/out" 2>&1; then
    echo "  ok    $rel"
  else
    status=1
    echo "  FAIL  $rel"
    sed 's/^/    /' "$tmp/out"
  fi
done
if [ "$status" != 0 ]; then
  echo "merge-tree-check: $base + $head does NOT build — each side may be green alone, the merge is not" >&2
fi
exit "$status"
