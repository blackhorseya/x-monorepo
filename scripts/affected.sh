#!/usr/bin/env bash
# Print the go.work modules affected by a change, one per line (e.g. "pkgs/ordering").
#
#   scripts/affected.sh                 # vs BASE_REF (default origin/main, falls back to HEAD)
#   scripts/affected.sh --files a b c   # from an explicit file list
#   scripts/affected.sh --all           # every module
#
# Rules:
#   - a file under a module dir affects that module
#   - a module affects every module that depends on it (reverse deps via go.work)
#   - a change outside any module (protos/, buf.gen.yaml, go.work, Taskfile.yml, .golangci.yml) affects all
#   - so does a change to what the test job itself runs on: .github/workflows/ci.yml,
#     .github/actions/* and scripts/* (the same inputs scripts/ci-changes.sh runs every job for)
set -euo pipefail
cd "$(dirname "$0")/.."

modules() { go work edit -json ./go.work | python3 -c 'import json,sys; [print(u["DiskPath"].removeprefix("./")) for u in json.load(sys.stdin)["Use"]]'; }

ALL=$(modules)

if [[ "${1:-}" == "--all" ]]; then echo "$ALL"; exit 0; fi

if [[ "${1:-}" == "--files" ]]; then
  shift; FILES=$(printf '%s\n' "$@")
else
  BASE="${BASE_REF:-origin/main}"
  git rev-parse --verify -q "$BASE" >/dev/null || BASE=HEAD
  FILES=$( (git diff --no-renames --name-only "$BASE"...HEAD 2>/dev/null || true; git diff --no-renames --name-only HEAD; git ls-files --others --exclude-standard) | sort -u)
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
      hpath=$(go mod edit -json "$h/go.mod" | python3 -c 'import json,sys; print(json.load(sys.stdin)["Module"]["Path"])')
      if grep -qF "$hpath " "$m/go.mod"; then mark "$m"; changed=1; break; fi
    done
  done
done

for m in $ALL; do is_hit "$m" && echo "$m"; done
true
