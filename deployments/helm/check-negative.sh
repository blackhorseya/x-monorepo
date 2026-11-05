#!/usr/bin/env bash
# Every ci-negative/*.yaml profile must fail `helm template`, and say why.
#   deployments/helm/check-negative.sh [chart-dir]
# The leading lines of each profile are `# expect: <text>` (one or more); the failure output has to contain every
# one of them, so a message that must name two ways out can be pinned to both. A profile that renders fine, or
# fails for another reason, fails this script.
set -euo pipefail
cd "$(dirname "$0")"
chart=${1:-x-monorepo}
rc=0
for f in "$chart"/ci-negative/*.yaml; do
  wants=()
  expects_rc=0; expects=$(sed -n '/^# expect: /!q;p' "$f") || expects_rc=$?
  [ "$expects_rc" = 0 ] || { echo "helm check-negative: $f: reading the '# expect:' lines exited $expects_rc"; rc=1; continue; }
  while IFS= read -r line; do [ -z "$line" ] || wants+=("${line#\# expect: }"); done <<<"$expects"
  [ "${#wants[@]}" -gt 0 ] || { echo "helm check-negative: $f: first line must be '# expect: <text>'"; rc=1; continue; }
  if out=$(helm template t "$chart" -f "$f" 2>&1); then
    echo "helm check-negative: $f rendered, want a failure naming '${wants[*]}'"; rc=1; continue
  fi
  missing=""
  for want in "${wants[@]}"; do
    case $out in *"$want"*) ;; *) missing="$missing '$want'" ;; esac
  done
  if [ -z "$missing" ]; then echo "helm check-negative $(basename "$f"): ok"
  else echo "helm check-negative: $f failed, but without$missing:"; echo "$out" | head -5; rc=1; fi
done
exit $rc
