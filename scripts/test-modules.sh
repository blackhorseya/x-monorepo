#!/usr/bin/env bash
# Run `go test -race ./...` in each module of one test-matrix group, every module to the end, then judge (#547).
#
#   MODULES_JSON='["pkgs/kernel","tools/newbc"]' scripts/test-modules.sh      # ci.yml's test job
#
# MODULES_JSON is `toJSON(matrix.group.modules)`: a non-empty JSON array of go.work module paths (scripts/affected.sh --matrix
# makes it). A path that go.work does not list is refused, and the list of what go.work allows comes from scripts/modules.jq,
# the one rule for a module path, so nothing here is spliced into a command unchecked.
#
# A red module does not stop the rest: its exit status is recorded, the red module name is printed right after that module's
# own output, and the summary at the end lists every module with its status. The script exits 1 when any module failed.
# The CI guarantee that pgtest fails (does not skip) when no container runtime is found rests on CI being set and
# TEST_DATABASE_URL not being set (pkgs/platform/postgres/pgtest): this script sets and unsets neither.
set -euo pipefail
self_dir=${0%/*}; [ "$self_dir" != "$0" ] || self_dir=.   # dirname without a $(…)
cd "$self_dir/.." || exit 1
. "$self_dir/best-effort.sh" || exit 1   # capture: a command's output next to its exit status

capture mods mods_rc jq -r 'if type == "array" and length > 0 and all(.[]; type == "string") then .[] else error("MODULES_JSON must be a non-empty JSON array of strings") end' <<< "${MODULES_JSON:-}" || exit 1
if [ "$mods_rc" != 0 ]; then echo "test-modules: MODULES_JSON is not a non-empty JSON array of module paths (jq exit $mods_rc)" >&2; exit 1; fi

capture work work_rc go work edit -json ./go.work || exit 1
if [ "$work_rc" != 0 ]; then echo "test-modules: go work edit failed (exit $work_rc)" >&2; exit 1; fi
capture known known_rc jq -r -f scripts/modules.jq <<< "$work" || exit 1
if [ "$known_rc" != 0 ]; then echo "test-modules: scripts/modules.jq refused go.work (exit $known_rc)" >&2; exit 1; fi

red=$'\033[31m' plain=$'\033[0m'
results="" failed=""
while IFS= read -r m; do
  case $'\n'"$known"$'\n' in
    *$'\n'"$m"$'\n'*) ;;
    *) echo "test-modules: $m is not a go.work module" >&2; exit 1 ;;
  esac
  echo "=== go test -race ./... in $m ==="
  rc=0
  ( cd "$m" && go test -race ./... ) || rc=$?
  if [ "$rc" = 0 ]; then
    results="$results  ok      $m"$'\n'
  else
    echo "${red}FAILED $m (exit $rc)${plain}"
    echo "::error title=go test failed::$m (exit $rc)"
    results="$results  FAILED  $m (exit $rc)"$'\n'
    failed="$failed $m"
  fi
done <<< "$mods"

echo "=== summary ==="
printf '%s' "$results"
if [ -n "$failed" ]; then
  echo "${red}FAILED modules:${failed}${plain}"
  exit 1
fi
