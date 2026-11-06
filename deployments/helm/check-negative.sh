#!/usr/bin/env bash
# Every ci-negative/*.yaml profile must fail `helm template`, and say why.
#   deployments/helm/check-negative.sh [chart-dir]
# The leading lines of each profile are `# expect: <text>` (one or more); the failure output has to contain every
# one of them, so a message that must name two ways out can be pinned to both. A profile that renders fine, or
# fails for another reason, fails this script.
set -euo pipefail
abs_dir_of() { local p=${1%/*}; [ "$p" != "$1" ] || p=.; ( cd "$p${2:+/$2}" && pwd ); }   # the absolute directory of a path, without dirname inside $(…)
here=$(abs_dir_of "$0") || exit 1
root=$(abs_dir_of "$0" ../..) || exit 1
. "$root/scripts/best-effort.sh"   # capture
cd "$here" || exit 1
chart=${1:-x-monorepo}
rc=0
for f in "$chart"/ci-negative/*.yaml; do
  wants=()
  capture expects expects_rc sed -n '/^# expect: /!q;p' "$f" || exit 1
  [ "$expects_rc" = 0 ] || { echo "helm check-negative: $f: reading the '# expect:' lines exited $expects_rc"; rc=1; continue; }
  while IFS= read -r line; do [ -z "$line" ] || wants+=("${line#\# expect: }"); done <<<"$expects"
  [ "${#wants[@]}" -gt 0 ] || { echo "helm check-negative: $f: first line must be '# expect: <text>'"; rc=1; continue; }
  out="" helm_rc=0
  capture -2 out helm_rc helm template t "$chart" -f "$f"   # a profile must FAIL: its non-zero status is expected, the text is what is judged
  if [ "$helm_rc" = 0 ]; then
    echo "helm check-negative: $f rendered, want a failure naming '${wants[*]}'"; rc=1; continue
  fi
  missing=""
  for want in "${wants[@]}"; do
    case $out in *"$want"*) ;; *) missing="$missing '$want'" ;; esac
  done
  if [ -z "$missing" ]; then echo "helm check-negative ${f##*/}: ok"
  else echo "helm check-negative: $f failed, but without$missing:"; echo "$out" | head -5; rc=1; fi
done
exit $rc
