#!/usr/bin/env bash
# Every module go.work `use`s must be a path Taskfile.yml's MODULES can carry (#413).
# MODULES is derived from go.work (Taskfile.yml: `go work edit -json | jq -f scripts/modules.jq`), so there is no
# second list to keep in sync and a module added to go.work is linted, tested and tidied without touching the
# Taskfile. scripts/modules.jq is the one rule for what a path may be (an allowlist, because the per-module loops
# splice the path into shell commands) and for an empty `use` list; MODULES runs it on every task, so a bad path fails
# every task locally. This script runs the same file on a given go.work, for `task check-work-sync` (the CI work-sync
# job) and for the tests; it keeps no rule of its own.
#
#   scripts/check-modules.sh [go.work]     # default: the repo root's
#
# Needs go and jq on PATH and fails without them; it never skips. Plain bash 3.2 (macOS) and 5 (ubuntu).
set -euo pipefail
abs_dir() { ( cd "$1" && pwd ); }   # an absolute directory without a `cd … && pwd` inside $(…) (#470)
root=$(abs_dir "$(dirname "$0")/..")
gowork=${1:-$root/go.work}

[ -r "$gowork" ] || { echo "check-modules: cannot read $gowork" >&2; exit 1; }
for tool in go jq; do
  command -v "$tool" >/dev/null || { echo "check-modules: $tool not found on PATH" >&2; exit 1; }
done

work_json=$(go work edit -json "$gowork") || { echo "check-modules: go work edit -json failed on $gowork" >&2; exit 1; }
modules=$(printf '%s\n' "$work_json" | jq -r -f "$root/scripts/modules.jq" 2>&1) || {
  echo "check-modules: $gowork is not a go.work MODULES can be derived from:" >&2
  printf '  %s\n' "$modules" >&2
  exit 1
}
n=$(printf '%s\n' "$modules" | wc -l | tr -d ' ')
echo "check-modules: go.work lists $n modules, all representable in MODULES"
