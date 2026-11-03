#!/usr/bin/env bash
# Taskfile.yml's MODULES must list exactly the modules go.work `use`s (#413).
# lint / test / cover / tidy loop over MODULES; go.work does not add a module to it, so a module only in go.work
# is never linted or tested while CI stays green (apps/payment-worker, #412). `task check-work-sync` runs this
# first; the CI work-sync job runs that (ci-changes.sh: go.work, */go.mod, *.go, Taskfile.yml, scripts/*).
#
#   scripts/check-modules.sh [go.work] [Taskfile.yml]     # defaults: the repo root's
#
# Neither side is parsed here: go.work is read by `go work edit -json` and MODULES by Task itself (the Taskfile's
# `modules` task prints {{.MODULES}}), so whatever YAML / template form Task accepts is compared as Task resolves
# it (#413 review: a text parser read only the first line of a multi-line MODULES). Needs go and task on PATH
# and fails without them; it never skips. Either side resolving to an empty set is a failure: empty == empty
# would be green with both readers broken. Plain bash 3.2 (macOS) and 5 (ubuntu).
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
gowork=${1:-$root/go.work}
taskfile=${2:-$root/Taskfile.yml}

[ -r "$gowork" ] || { echo "check-modules: cannot read $gowork" >&2; exit 1; }
[ -r "$taskfile" ] || { echo "check-modules: cannot read $taskfile" >&2; exit 1; }
command -v go >/dev/null || { echo "check-modules: go not found on PATH" >&2; exit 1; }
command -v task >/dev/null || { echo "check-modules: task not found on PATH" >&2; exit 1; }

# words: one module per line, sorted, "./" and trailing "/" dropped.
words() { sed 's#^\./##; s#/$##' | sed '/^$/d' | sort -u; }

work_json=$(go work edit -json "$gowork") || { echo "check-modules: go work edit -json failed on $gowork" >&2; exit 1; }
in_work=$(printf '%s\n' "$work_json" | sed -n 's/^[[:space:]]*"DiskPath":[[:space:]]*"\(.*\)",\{0,1\}$/\1/p' | words)
task_out=$(task --silent --taskfile "$taskfile" --dir "$(cd "$(dirname "$taskfile")" && pwd)" modules 2>&1) || {
  echo "check-modules: task modules failed on $taskfile (it needs a 'modules' task that prints {{.MODULES}}): $task_out" >&2
  exit 1
}
in_task=$(printf '%s\n' "$task_out" | tr ' \t' '\n\n' | words)

n_work=$(printf '%s' "$in_work" | grep -c . || true)
n_task=$(printf '%s' "$in_task" | grep -c . || true)
rc=0
[ "$n_work" -gt 0 ] || { echo "check-modules: go.work lists no modules ($gowork)" >&2; rc=1; }
[ "$n_task" -gt 0 ] || { echo "check-modules: MODULES lists no modules ($taskfile)" >&2; rc=1; }
[ "$rc" = 0 ] || exit 1

only_work=$(comm -23 <(printf '%s\n' "$in_work") <(printf '%s\n' "$in_task") | tr '\n' ' ' | sed 's/ $//')
only_task=$(comm -13 <(printf '%s\n' "$in_work") <(printf '%s\n' "$in_task") | tr '\n' ' ' | sed 's/ $//')
if [ -n "$only_work" ]; then echo "check-modules: in go.work but not in MODULES: $only_work (never linted or tested; add to Taskfile.yml MODULES)" >&2; rc=1; fi
if [ -n "$only_task" ]; then echo "check-modules: in MODULES but not in go.work: $only_task (add to go.work or drop from MODULES)" >&2; rc=1; fi
[ "$rc" = 0 ] || exit 1
echo "check-modules: go.work and MODULES agree ($n_work modules)"
