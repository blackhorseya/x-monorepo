#!/usr/bin/env bash
# Coverage gate: every */domain/* package must be 100% covered.
# Domain is pure logic with no I/O, so anything below 100% is an untested rule.
set -euo pipefail
_lib=${BASH_SOURCE[0]%/*}; [ "$_lib" != "${BASH_SOURCE[0]}" ] || _lib=.   # this script's own directory, whatever the cwd (parameter expansion only)
. "$_lib/best-effort.sh" || { echo "cover-gate: cannot load $_lib/best-effort.sh (value_of)" >&2; exit 2; }   # a missing library stops the script (#470)
cd "$(dirname "$0")/.."
mkdir -p bin

fail=0 checked=0
# each step on its own line: `for m in $(go … | python3 …)` and `go list … || true` turned a go that failed into an empty
# module list or a module without domain packages — and a gate that checked nothing exited 0 (#462)
workjson=$(go work edit -json ./go.work)
mods=$(python3 -c 'import json,sys; [print(u["DiskPath"].removeprefix("./")) for u in json.load(sys.stdin)["Use"]]' <<<"$workjson")
[[ -n "$mods" ]] || { echo "cover-gate: go.work lists no module — nothing was checked" >&2; exit 1; }
run_in() { local d=$1; shift; ( cd "$d" && "$@" ); }   # a command in another directory, without `cd … && cmd` inside $(…) (#470)
for m in $mods; do
  list=$(run_in "$m" go list ./...) || { echo "cover-gate: go list failed in $m" >&2; exit 1; }
  pkgs=$(value_of -ok 1 grep '/domain/' <<<"$list") || exit 1
  [[ -z "$pkgs" ]] && continue
  for p in $pkgs; do
    checked=$((checked + 1))
    prof="bin/cover-$(echo "$p" | tr '/' '_').out"
    go test -coverprofile="$prof" "$p" >/dev/null
    pct=$(go tool cover -func="$prof" | tail -1 | awk '{print $3}' | tr -d '%')
    if [[ "$pct" != "100.0" ]]; then
      echo "FAIL domain coverage $p = $pct% (want 100%)"; fail=1
      go tool cover -func="$prof" | awk '$3 != "100.0%" && $1 != "total:" {print "   " $0}'
    else
      echo "ok   $p 100%"
    fi
  done
done
[[ "$checked" -gt 0 ]] || { echo "cover-gate: no */domain/* package in any module — nothing was checked" >&2; exit 1; }
exit $fail
