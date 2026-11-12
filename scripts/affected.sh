#!/usr/bin/env bash
# Print the go.work modules affected by a change, one per line (e.g. "pkgs/platform").
#
#   scripts/affected.sh                 # vs BASE_REF (default origin/main, falls back to HEAD)
#   scripts/affected.sh --files a b c   # from an explicit file list
#   scripts/affected.sh --all           # every module
#   scripts/affected.sh --matrix [--all | --files a b c]
#                                       # the same modules as ci.yml's test matrix groups, one JSON array (#547)
#
# Rules:
#   - a file under a module dir affects that module
#   - a module affects every module that depends on it (reverse deps via go.work)
#   - a change outside any module (protos/, buf.gen.yaml, go.work, Taskfile.yml, .golangci.yml) affects all
#   - so does a change to what the test job itself runs on: .github/workflows/ci.yml,
#     .github/actions/* and scripts/* (the same inputs scripts/ci-changes.sh runs every job for)
#
# --matrix (#547): the test job runs a few big modules one job each (STANDALONE) and every other affected module in ONE shared
# job, so the fixed cost of a job (setup, image pre-pull) is paid once for the small ones. The groups are derived from what
# this script already printed: shared = affected − STANDALONE. There is no second list of modules to keep in step with
# go.work: a name here that is not a module is never matched, and a module go.work gains that is not named here lands in the
# shared group, so it is always tested. Output, one JSON array (empty when nothing is affected), standalone entries first:
#   [{"name":"apps/rpc","modules":["apps/rpc"]},{"name":"(shared)","modules":["pkgs/kernel","tools/newbc"]}]
# "(shared)" holds characters scripts/modules.jq refuses in a module path, so it can never be a module's name. `name` is the
# job's cache-scope suffix (test-<name>, mapped to a build-cache group in .github/actions/setup/plan.sh).
#
# STANDALONE: keep apps/rpc here. plan.sh designates the scope test-apps/rpc as the one job that saves the shared "race" build
# cache (the largest compile set); without this entry no job would save it.
STANDALONE="apps/rpc apps/worker pkgs/platform"
set -euo pipefail
self_dir=${0%/*}; [ "$self_dir" != "$0" ] || self_dir=.   # dirname without a $(…)
cd "$self_dir/.." || exit 1
. "$self_dir/best-effort.sh" || exit 1   # capture: the status of git diff next to what it printed

modules() { go work edit -json ./go.work | python3 -c 'import json,sys; [print(u["DiskPath"].removeprefix("./")) for u in json.load(sys.stdin)["Use"]]'; }

if [[ "${1:-}" == "--matrix" ]]; then
  shift
  # the affected list comes from this very script, run again without the flag; a failure of it is a failure of --matrix, never `[]`
  capture mods mods_rc bash "$self_dir/affected.sh" "$@" || exit 1
  if [ "$mods_rc" != 0 ]; then echo "affected: --matrix: the module list failed (exit $mods_rc)" >&2; exit "$mods_rc"; fi
  capture groups groups_rc jq -Rsc --arg standalone "$STANDALONE" '
    ($standalone | split(" ") | map(select(length > 0))) as $s
    | (split("\n") | map(select(length > 0))) as $m
    | [ $m[] | select(. as $x | $s | index($x)) | {name: ., modules: [.]} ]
      + ([ $m[] | select(. as $x | $s | index($x) | not) ] | if length > 0 then [{name: "(shared)", modules: .}] else [] end)' <<< "$mods" || exit 1
  if [ "$groups_rc" != 0 ]; then echo "affected: --matrix: jq failed (exit $groups_rc)" >&2; exit "$groups_rc"; fi
  printf '%s\n' "$groups"
  exit 0
fi

ALL=$(modules) || exit $?

if [[ "${1:-}" == "--all" ]]; then echo "$ALL"; exit 0; fi

if [[ "${1:-}" == "--files" ]]; then
  shift; FILES=$(printf '%s\n' "$@") || exit $?
else
  BASE="${BASE_REF:-origin/main}"
  git rev-parse --verify -q "$BASE" >/dev/null || BASE=HEAD
  # No merge base (a shallow clone) makes the three-dot diff fail. That used to read as "nothing changed on the branch" (|| true)
  # and shrink the test matrix; now it means "cannot tell", and every module is affected (#462).
  capture committed committed_rc git diff --no-renames --name-only "$BASE"...HEAD 2>/dev/null || exit 1
  if [ "$committed_rc" != 0 ]; then
    echo "affected: git diff $BASE...HEAD failed — treating every module as affected" >&2
    echo "$ALL"; exit 0
  fi
  # one assignment each: a failing git stops the script here, it is not a shorter list
  worktree=$(git diff --no-renames --name-only HEAD) || exit $?
  untracked=$(git ls-files --others --exclude-standard) || exit $?
  FILES=$(printf '%s\n%s\n%s\n' "$committed" "$worktree" "$untracked" | sort -u) || exit $?
fi

[[ -z "$FILES" ]] && exit 0

# bash 3.2 (macOS /bin/bash) has no associative arrays: track hits as a space-delimited set.
HIT=" "
is_hit() { [[ "$HIT" == *" $1 "* ]]; }
mark()   { is_hit "$1" || HIT="$HIT$1 "; }

for f in $FILES; do
  matched=""
  for m in $ALL; do
    [[ "$f" == "$m/"* ]] && { mark "$m"; matched=1; break; }
  done
  # global inputs → everything
  if [[ -z "$matched" ]] && [[ "$f" == protos/* || "$f" == buf.* || "$f" == go.work || "$f" == Taskfile.yml || "$f" == .golangci.yml || "$f" == .github/workflows/ci.yml || "$f" == .github/actions/* || "$f" == scripts/* ]]; then
    echo "$ALL"; exit 0
  fi
done

# expand to reverse dependencies: a module that requires a hit module is also hit
changed=1
while [[ $changed == 1 ]]; do
  changed=0
  for m in $ALL; do
    is_hit "$m" && continue
    for h in $HIT; do
      hpath=$(go mod edit -json "$h/go.mod" | python3 -c 'import json,sys; print(json.load(sys.stdin)["Module"]["Path"])') || exit $?
      if grep -qF "$hpath " "$m/go.mod"; then mark "$m"; changed=1; break; fi
    done
  done
done

for m in $ALL; do is_hit "$m" && echo "$m"; done
true
