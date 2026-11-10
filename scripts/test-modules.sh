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

# go.work's module list first (scripts/modules.jq is the one rule for what a module path may be), as a JSON array for jq.
capture work work_rc go work edit -json ./go.work || exit 1
if [ "$work_rc" != 0 ]; then echo "test-modules: go work edit failed (exit $work_rc)" >&2; exit 1; fi
capture known known_rc jq -r -f scripts/modules.jq <<< "$work" || exit 1
if [ "$known_rc" != 0 ]; then echo "test-modules: scripts/modules.jq refused go.work (exit $known_rc)" >&2; exit 1; fi
capture known_json known_json_rc jq -R -s -c 'split("\n") | map(select(length > 0))' <<< "$known" || exit 1
if [ "$known_json_rc" != 0 ]; then echo "test-modules: could not read the go.work module list (jq exit $known_json_rc)" >&2; exit 1; fi

# ALL the validation is done by jq, on the JSON itself, before anything is split into lines: an element holding a newline would otherwise come out of
# `.[]` as two lines, i.e. two runs of one module. -s: the input must be exactly ONE JSON value (without it jq validates every value of `["a"] ["a"]`
# on its own and the repeat check never sees the two together). An element passes only when it is a string equal to a go.work module (so it already
# satisfies modules.jq's allowlist; there is no second copy of that rule here); the list must be non-empty with no repeated element. Any failure is
# non-zero with the offending value JSON-quoted, and nothing has run yet. The output is one module per line, and no module contains a newline.
capture mods mods_rc jq -r -s --argjson known "$known_json" '
  if length != 1 then error("MODULES_JSON must be exactly one JSON value, got " + (length | tostring))
  else .[0]
    | if type != "array" or length == 0 then error("MODULES_JSON must be a non-empty JSON array")
      elif (unique | length) != length then error("MODULES_JSON has a repeated element")
      else .[] | . as $m
        | if type == "string" and any($known[]; . == $m) then $m
          else error("MODULES_JSON element " + ($m | @json) + " is not a go.work module") end
      end
  end' <<< "${MODULES_JSON:-}" || exit 1
if [ "$mods_rc" != 0 ]; then echo "test-modules: MODULES_JSON refused (jq exit $mods_rc), no module was run" >&2; exit 1; fi
# jq prints nothing and exits 0 for empty or whitespace-only input; with no module line the loop below would run once with an empty path
if [ -z "$mods" ]; then echo "test-modules: MODULES_JSON gave no module, no module was run" >&2; exit 1; fi

red=$'\033[31m' plain=$'\033[0m'
results="" failed=""
while IFS= read -r m; do
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
