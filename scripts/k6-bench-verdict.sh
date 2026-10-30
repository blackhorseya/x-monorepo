#!/usr/bin/env bash
# What a k6-bench workflow run (.github/workflows/k6-bench.yml, #280) means, from the exit code of
# tests/k6/bench.sh: 0 = RUNS valid attempts; 3 = fewer valid attempts than RUNS ("NOT A VALID
# BASELINE", recorded, not a baseline); 99 = a k6 correctness threshold was crossed (or unclassifiable), a real failure; anything else = the bench itself failed; empty = the bench
# step never finished. Writes <out>/verdict.txt, appends it and <out>/summary.md to
# $GITHUB_STEP_SUMMARY when set, and exits 0 only for a valid baseline — every other case exits 1
# so the job is red and no invalid number can read as a success.
#   scripts/k6-bench-verdict.sh <exit code of bench.sh, or empty> <out dir>
set -euo pipefail
rc=${1-}; out=${2:?usage: k6-bench-verdict.sh <rc> <out dir>}
case "$rc" in
  0)  verdict="VALID: bench.sh found enough valid attempts (exit 0)" ;;
  3)  verdict="INVALID: NOT A VALID BASELINE — fewer valid attempts than RUNS (bench.sh exit 3). Do not write these numbers into docs/perf-baseline.md" ;;
  99) verdict="INVALID: bench.sh failed (exit 99): a k6 threshold that is not overload was crossed (or which one could not be determined) — a real failure, not an invalid attempt. No baseline" ;;
  "") verdict="INVALID: the bench step did not finish (cancelled, timed out, or failed before it ran). No baseline" ;;
  *)  verdict="INVALID: bench.sh failed (exit $rc). No baseline" ;;
esac
mkdir -p "$out"
echo "$verdict" | tee "$out/verdict.txt"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "## k6-bench: $verdict"
    if [ -f "$out/summary.md" ]; then cat "$out/summary.md"; else echo "(no summary.md was written)"; fi
  } >> "$GITHUB_STEP_SUMMARY"
fi
if [ "$rc" != 0 ]; then
  echo "::error title=k6-bench is not a valid baseline::$verdict"
  exit 1
fi
