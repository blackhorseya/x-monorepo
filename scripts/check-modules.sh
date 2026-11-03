#!/usr/bin/env bash
# Taskfile.yml's MODULES must list exactly the modules go.work `use`s (#413).
# lint / test / cover / tidy loop over MODULES; go.work does not add a module to it, so a module only in go.work
# is never linted or tested while CI stays green (apps/payment-worker, #412). `task check-work-sync` runs this
# first; the CI work-sync job runs that (ci-changes.sh: go.work, */go.mod, *.go, Taskfile.yml, scripts/*).
#
#   scripts/check-modules.sh [go.work] [Taskfile.yml]     # defaults: the repo root's
#
# Either side parsing to an empty set is a failure: empty == empty would be green with both parsers broken.
# Plain bash 3.2 (macOS) and 5 (ubuntu).
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
gowork=${1:-$root/go.work}
taskfile=${2:-$root/Taskfile.yml}

[ -r "$gowork" ] || { echo "check-modules: cannot read $gowork" >&2; exit 1; }
[ -r "$taskfile" ] || { echo "check-modules: cannot read $taskfile" >&2; exit 1; }

# words: one module per line, sorted, "./" and trailing "/" dropped.
words() { sed 's#^\./##; s#/$##' | sed '/^$/d' | sort -u; }

# go.work: `use ./x` and the `use ( ... )` block; // comments dropped.
in_work=$(awk '
  { sub(/\/\/.*/, "") }
  inblock && /^[ \t]*\)/ { inblock = 0; next }
  inblock { for (i = 1; i <= NF; i++) print $i; next }
  $1 == "use" && $2 == "(" { inblock = 1; next }
  $1 == "use" { for (i = 2; i <= NF; i++) print $i }
' "$gowork" | words)
# Taskfile.yml: the one `MODULES:` var, # comment dropped.
in_task=$(sed -n 's/^[[:space:]]*MODULES:[[:space:]]*//p' "$taskfile" | sed 's/[[:space:]]*#.*//' | tr ' ' '\n' | words)

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
