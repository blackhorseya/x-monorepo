#!/usr/bin/env bash
# Every module go.work `use`s must be a path Taskfile.yml's MODULES can carry (#413).
# MODULES is derived from go.work (Taskfile.yml: `go work edit -json | jq`), so there is no second list to keep in
# sync and a module added to go.work is linted, tested and tidied without touching the Taskfile. What the derivation
# cannot carry is a path holding whitespace or a control character: `for: { var: MODULES, split: "\n" }` loops
# would cut it in two or hand `cd` a word list. Such a go.work path is refused here, shown JSON-quoted, and an
# empty `use` list is refused (every per-module task would iterate nothing and pass). `task check-work-sync` runs
# this (the CI work-sync job).
#
#   scripts/check-modules.sh [go.work]     # default: the repo root's
#
# Needs go and jq on PATH and fails without them; it never skips. Plain bash 3.2 (macOS) and 5 (ubuntu).
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
gowork=${1:-$root/go.work}

[ -r "$gowork" ] || { echo "check-modules: cannot read $gowork" >&2; exit 1; }
for tool in go jq; do
  command -v "$tool" >/dev/null || { echo "check-modules: $tool not found on PATH" >&2; exit 1; }
done

work_json=$(go work edit -json "$gowork") || { echo "check-modules: go work edit -json failed on $gowork" >&2; exit 1; }
# The refusal is decided in jq, on the decoded path (a text match on the JSON would see "\n" as two characters).
unrepresentable=$(printf '%s\n' "$work_json" | jq -r '(.Use // [])[].DiskPath | select(test("[\\s[:cntrl:]]")) | @json') || { echo "check-modules: jq could not read the go.work use list" >&2; exit 1; }
if [ -n "$unrepresentable" ]; then
  echo "check-modules: go.work uses a path with whitespace or control characters; a for-loop over MODULES (one path per line, split on \"\\n\") cannot carry it:" >&2
  printf '  %s\n' "$unrepresentable" >&2
  exit 1
fi
n=$(printf '%s\n' "$work_json" | jq -r '(.Use // []) | length') || { echo "check-modules: jq could not read the go.work use list" >&2; exit 1; }
if [ "$n" -eq 0 ]; then
  echo "check-modules: go.work lists no modules ($gowork)" >&2
  exit 1
fi
echo "check-modules: go.work lists $n modules, all representable in MODULES"
