#!/usr/bin/env bash
# The console-e2e shards are the whole suite (#447): `playwright test --list --shard=i/N` for i in 1..N, put together,
# is exactly the unsharded `--list` (same test ids, none twice, none missing). Lists only, starts no server.
#
#   cd web/apps/console && ../../../scripts/web-e2e-shards-check.sh <N>     # task web-e2e-shards-check -- <N>
#
# Every `--list` must exit 0 and its own "Total: K tests" line must equal the test lines it printed: a list that died
# half way (non-zero exit, or a truncated output) is a failure, never a smaller suite that still "matches".
# Exit: 0 shards = suite, 1 they differ or a list failed, 2 usage. Test: scripts/test.sh (a stub pnpm with faults).
set -euo pipefail
# Every external command is checked. `set -e` does not see a failure inside `$(...)` of a `for` word list, nor one
# swallowed by `|| true`; grep's exit 1 (no match) is the only non-zero this script accepts, and then only where said.
n="${1:-}"
case "$n" in "" | *[!0-9]* | 0) echo "usage: web-e2e-shards-check.sh <number of shards>" >&2; exit 2 ;; esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# list <name> [playwright args]: tests (the lines with the › separator) of one --list run into $tmp/<name>.
list() {
  local name=$1 rc=0 want have
  shift
  pnpm exec playwright test --list "$@" >"$tmp/$name.raw" 2>"$tmp/$name.err" || rc=$?
  if [ "$rc" != 0 ]; then
    echo "playwright test --list $* exited $rc" >&2; cat "$tmp/$name.err" >&2; exit 1
  fi
  rc=0; grep '›' "$tmp/$name.raw" >"$tmp/$name" || rc=$?
  if [ "$rc" -gt 1 ]; then echo "grep over the playwright --list output exited $rc" >&2; exit 1; fi   # 1 = no test line: the Total check below says so
  want=$(sed -n 's/^Total: \([0-9][0-9]*\) tests\{0,1\} .*/\1/p' "$tmp/$name.raw")
  have=$(wc -l <"$tmp/$name" | tr -d ' ')
  if [ -z "$want" ] || [ "$want" != "$have" ]; then
    echo "playwright test --list $*: its Total says '${want:-none}' but it printed $have tests (truncated output?)" >&2; exit 1
  fi
  [ "$have" -gt 0 ] || { echo "playwright test --list $* lists no test" >&2; exit 1; }
}

list full
sort "$tmp/full" >"$tmp/full.sorted"
: >"$tmp/union"
sum=0
seq "$n" >"$tmp/indexes" || { echo "seq $n exited $?" >&2; exit 1; }
for i in $(cat "$tmp/indexes"); do
  list "shard$i" --shard="$i/$n"
  c=$(wc -l <"$tmp/shard$i" | tr -d ' ')
  echo "shard $i/$n tests: $c"
  sum=$((sum + c))
  cat "$tmp/shard$i" >>"$tmp/union"
done
sort "$tmp/union" >"$tmp/union.sorted"
total=$(wc -l <"$tmp/full" | tr -d ' ')
if [ "$sum" != "$total" ] || ! diff "$tmp/full.sorted" "$tmp/union.sorted" >&2; then
  echo "the $n shards ($sum tests) are not the whole suite ($total tests)" >&2; exit 1
fi
echo "ok: $n shards = $total tests, each exactly once"
