#!/usr/bin/env bash
# Run the dependency sync until a round changes nothing (#478). dependabot-sync.yml used to run `task tidy` once; for #475 (mvdan.cc/sh 3.13.1 ->
# 3.14.1) that round added go-internal to 12 go.mod and the NEXT round took it from 11 of them, so the bot committed a tree that was not a fixed
# point and check-work-sync was red until a human ran the second round.
#
#   scripts/sync-fixpoint.sh [max_rounds]      # default 5; SYNC_FIXPOINT_CMD overrides the sync command (default: task tidy)
#
# One round = the sync command. After each round the module files are compared with the round before; a round that changed nothing is the fixed
# point (exit 0), so a tree that needed k changing rounds takes k + 1 rounds to prove. More than max_rounds rounds, or a round whose command fails,
# exits 1 and leaves the tree as it is: the workflow's commit step is after this one and never runs. 2 = bad argument.
# What `task tidy` fixes is a superset of what check-work-sync judges: tidy = `go mod tidy` per module, then `go work sync`; check-work-sync = `go work sync`
# on the committed tree and no drift in go.work / go.mod (not go.sum). A fixed point of tidy ends with a `go work sync` that changed nothing, so check-work-sync is green.
set -euo pipefail

max=${1:-5}
case "$max" in '' | *[!0-9]* | 0) echo "sync-fixpoint: max rounds must be a positive integer, got [$max]" >&2; exit 2 ;; esac
sync_cmd=${SYNC_FIXPOINT_CMD:-task tidy}

# snapshot_files: the content of every file the sync may change, with its name. The same five pathspecs dependabot-sync.yml diffs and commits.
snapshot_files() {
  local pat f
  for pat in go.work '*/go.mod' '*/*/go.mod' '*/go.sum' '*/*/go.sum'; do
    for f in $pat; do
      if [ -f "$f" ]; then printf '== %s\n' "$f"; cat "$f"; fi
    done
  done
}

before=$(snapshot_files | cksum) || { echo "sync-fixpoint: cannot read the module files" >&2; exit 1; }
round=0
while :; do
  round=$((round + 1))
  if [ "$round" -gt "$max" ]; then echo "sync-fixpoint: no fixed point after $max rounds" >&2; exit 1; fi
  echo "sync-fixpoint: round $round: $sync_cmd"
  bash -c "$sync_cmd" || { echo "sync-fixpoint: round $round failed (exit $?)" >&2; exit 1; }
  after=$(snapshot_files | cksum) || { echo "sync-fixpoint: cannot read the module files after round $round" >&2; exit 1; }
  if [ "$after" = "$before" ]; then echo "sync-fixpoint: fixed point after $round round(s)"; exit 0; fi
  before=$after
done
