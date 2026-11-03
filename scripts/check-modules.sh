#!/usr/bin/env bash
# Taskfile.yml's MODULES must list exactly the modules go.work `use`s (#413).
# lint / test / cover / tidy loop over MODULES; go.work does not add a module to it, so a module only in go.work
# is never linted or tested while CI stays green (apps/payment-worker, #412). `task check-work-sync` runs this
# first; the CI work-sync job runs that (ci-changes.sh: go.work, */go.mod, *.go, Taskfile.yml, scripts/*).
#
#   scripts/check-modules.sh [go.work] [Taskfile.yml]     # defaults: the repo root's
#
# Neither side is parsed here: go.work is read by `go work edit -json` and decoded by jq, and MODULES by Task
# itself (the Taskfile's `modules` task prints {{.MODULES}}), so whatever YAML / template / JSON-escape form the
# tools accept is compared as they resolve it (#413 reviews: a text parser read only the first line of a
# multi-line MODULES; sed on the JSON left "\u0026" in a path). The only string handling left is splitting
# Task's printed list on whitespace and dropping a path's "./" prefix. Needs go, task and jq on PATH and fails
# without them; it never skips. Either side resolving to an empty set is a failure: empty == empty would be
# green with both readers broken. Plain bash 3.2 (macOS) and 5 (ubuntu).
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
gowork=${1:-$root/go.work}
taskfile=${2:-$root/Taskfile.yml}

[ -r "$gowork" ] || { echo "check-modules: cannot read $gowork" >&2; exit 1; }
[ -r "$taskfile" ] || { echo "check-modules: cannot read $taskfile" >&2; exit 1; }
for tool in go task jq; do
  command -v "$tool" >/dev/null || { echo "check-modules: $tool not found on PATH" >&2; exit 1; }
done

# canon: one module per line from stdin, "./" prefix and trailing "/" dropped, sorted, unique.
canon() {
  local p
  while IFS= read -r p; do
    p=${p#./}; p=${p%/}
    [ -z "$p" ] || printf '%s\n' "$p"
  done | sort -u
}

work_json=$(go work edit -json "$gowork") || { echo "check-modules: go work edit -json failed on $gowork" >&2; exit 1; }
# MODULES is a whitespace-separated list (Task's `for: { var: MODULES }` splits it on whitespace), so it cannot name a
# module whose path holds whitespace or a control character: refuse such a go.work path, shown JSON-quoted so its
# boundaries and escapes are visible. After this no path holds a newline, so the line-per-module sets below are exact
# (#413 r3: a newline inside a path was read as two modules and "apps/a\nb" agreed with MODULES "apps/a b").
unrepresentable=$(printf '%s\n' "$work_json" | jq -r '(.Use // [])[].DiskPath | select(test("[\\s[:cntrl:]]")) | @json') || { echo "check-modules: jq could not read the go.work use list" >&2; exit 1; }
if [ -n "$unrepresentable" ]; then
  echo "check-modules: go.work uses a path with whitespace or control characters; MODULES is whitespace-separated and cannot represent it:" >&2
  printf '  %s\n' "$unrepresentable" >&2
  exit 1
fi
in_work=$(printf '%s\n' "$work_json" | jq -r '(.Use // [])[].DiskPath' | canon) || { echo "check-modules: jq could not read the go.work use list" >&2; exit 1; }
task_out=$(task --silent --taskfile "$taskfile" --dir "$(cd "$(dirname "$taskfile")" && pwd)" modules 2>&1) || {
  echo "check-modules: task modules failed on $taskfile (it needs a 'modules' task that prints {{.MODULES}}): $task_out" >&2
  exit 1
}
in_task=$(printf '%s\n' "$task_out" | tr ' \t' '\n\n' | canon)

# count: number of lines in $1 (0 for the empty string).
count() { if [ -z "$1" ]; then echo 0; else printf '%s\n' "$1" | wc -l | tr -d ' '; fi; }
n_work=$(count "$in_work")
n_task=$(count "$in_task")
rc=0
[ "$n_work" -gt 0 ] || { echo "check-modules: go.work lists no modules ($gowork)" >&2; rc=1; }
[ "$n_task" -gt 0 ] || { echo "check-modules: MODULES lists no modules ($taskfile)" >&2; rc=1; }
[ "$rc" = 0 ] || exit 1

quote() { local p out=""; while IFS= read -r p; do out="$out\"$p\" "; done; printf '%s' "${out% }"; }
only_work=$(comm -23 <(printf '%s\n' "$in_work") <(printf '%s\n' "$in_task") | quote)
only_task=$(comm -13 <(printf '%s\n' "$in_work") <(printf '%s\n' "$in_task") | quote)
if [ -n "$only_work" ]; then echo "check-modules: in go.work but not in MODULES: $only_work (never linted or tested; add to Taskfile.yml MODULES)" >&2; rc=1; fi
if [ -n "$only_task" ]; then echo "check-modules: in MODULES but not in go.work: $only_task (add to go.work or drop from MODULES)" >&2; rc=1; fi
[ "$rc" = 0 ] || exit 1
echo "check-modules: go.work and MODULES agree ($n_work modules)"
