#!/usr/bin/env bash
# Coverage gate: every */domain/* package must be 100% covered.
# Domain is pure logic with no I/O, so anything below 100% is an untested rule.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p bin

fail=0
for m in $(go work edit -json ./go.work | python3 -c 'import json,sys; [print(u["DiskPath"].removeprefix("./")) for u in json.load(sys.stdin)["Use"]]'); do
  pkgs=$(cd "$m" && go list ./... 2>/dev/null | grep '/domain/' || true)
  [[ -z "$pkgs" ]] && continue
  for p in $pkgs; do
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
exit $fail
