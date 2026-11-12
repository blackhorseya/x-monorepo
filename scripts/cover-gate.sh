#!/usr/bin/env bash
# Coverage gate: every */domain/* package must be 100% covered.
# Domain is pure logic with no I/O, so anything below 100% is an untested rule.
set -euo pipefail
_lib=${BASH_SOURCE[0]%/*}; [ "$_lib" != "${BASH_SOURCE[0]}" ] || _lib=.   # this script's own directory, whatever the cwd (parameter expansion only)
. "$_lib/best-effort.sh" || { echo "cover-gate: cannot load $_lib/best-effort.sh (value_of)" >&2; exit 2; }   # a missing library stops the script (#470)
self_dir=${0%/*}; [ "$self_dir" != "$0" ] || self_dir=.   # dirname without a $(…)
cd "$self_dir/.." || exit 1
# where the coverage profiles go: ./bin by default (git-ignored); COVER_PROFILE_DIR overrides it, an absolute path, or one relative to the repo root
# (new-bc-check points it into its own temp dir so a run never writes into, or shares profiles through, the repo's bin). Empty means unset.
profile_dir=${COVER_PROFILE_DIR:-bin}
mkdir -p -- "$profile_dir"

fail=0 checked=0
# each step on its own line: `for m in $(go … | python3 …)` and `go list … || true` turned a go that failed into an empty
# module list or a module without domain packages — and a gate that checked nothing exited 0 (#462)
workjson=$(go work edit -json ./go.work) || exit $?
mods=$(python3 -c 'import json,sys; [print(u["DiskPath"].removeprefix("./")) for u in json.load(sys.stdin)["Use"]]' <<<"$workjson") || exit $?
[[ -n "$mods" ]] || { echo "cover-gate: go.work lists no module — nothing was checked" >&2; exit 1; }
run_in() { local d=$1; shift; ( cd "$d" && "$@" ); }   # a command in another directory, without `cd … && cmd` inside $(…) (#470)
for m in $mods; do
  list=$(run_in "$m" go list ./...) || { echo "cover-gate: go list failed in $m" >&2; exit 1; }
  pkgs=$(value_of -ok 1 grep '/domain/' <<<"$list") || exit 1
  [[ -z "$pkgs" ]] && continue
  for p in $pkgs; do
    checked=$((checked + 1))
    prof="$profile_dir/cover-${p//\//_}.out"
    go test -coverprofile="$prof" "$p" >/dev/null
    pct=$(go tool cover -func="$prof" | tail -1 | awk '{print $3}' | tr -d '%') || exit $?
    if [[ "$pct" != "100.0" ]]; then
      echo "FAIL domain coverage $p = $pct% (want 100%)"; fail=1
      go tool cover -func="$prof" | awk '$3 != "100.0%" && $1 != "total:" {print "   " $0}'
    else
      echo "ok   $p 100%"
    fi
  done
done
if [[ "$checked" -eq 0 ]]; then
  # a BC is every pkgs/<name> but kernel and platform: with none there is no domain to cover, and that is the template as it
  # ships; with a BC but no */domain/* package the glob or the layout is wrong, and a gate that checked nothing must not pass.
  # The pkgs/ directories are deliberately used instead of scripts/bcs.jq: they are a superset of go.work's BC set, so when the
  # two disagree the gate can only turn red, never pass.
  bcs=0
  for d in pkgs/*/; do
    d=${d%/}; [[ "${d##*/}" == kernel || "${d##*/}" == platform ]] || bcs=$((bcs + 1))
  done
  if [[ "$bcs" -eq 0 ]]; then echo "cover-gate: no bounded context under pkgs/, so no */domain/* package to check"; exit 0; fi
  echo "cover-gate: $bcs bounded context(s) but no */domain/* package in any module — nothing was checked" >&2; exit 1
fi
exit $fail
