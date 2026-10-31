#!/usr/bin/env bash
# Every ci-negative/*.yaml profile must fail `helm template`, and say why.
#   deployments/helm/check-negative.sh [chart-dir]
# The first line of each profile is `# expect: <text>`; the failure output has to contain it. A
# profile that renders fine, or fails for another reason, fails this script.
set -euo pipefail
cd "$(dirname "$0")"
chart=${1:-x-monorepo}
rc=0
for f in "$chart"/ci-negative/*.yaml; do
  want=$(sed -n '1s/^# expect: //p' "$f")
  [ -n "$want" ] || { echo "helm check-negative: $f: first line must be '# expect: <text>'"; rc=1; continue; }
  if out=$(helm template t "$chart" -f "$f" 2>&1); then
    echo "helm check-negative: $f rendered, want a failure naming '$want'"; rc=1; continue
  fi
  case $out in
    *"$want"*) echo "helm check-negative $(basename "$f"): ok" ;;
    *) echo "helm check-negative: $f failed, but not with '$want':"; echo "$out" | head -5; rc=1 ;;
  esac
done
exit $rc
