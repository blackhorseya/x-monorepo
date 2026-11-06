# shellcheck shell=bash
# The verdict logic of image-mutants.sh, apart from the part that starts containers, so that e2e/image-mutants.selftest.ts can
# feed it logs and exit codes (and fake sed / grep / sort / uniq / awk that fail half way). Sourced, never run.
set -o pipefail

# failed_specs <log>: the failed [image] spec files of a Playwright log as "file×count, file×count" on stdout.
# Non-zero if ANY stage of the pipeline fails (grep's exit 1 is "no failed test", a result; every other code is an error).
failed_specs() {
  sed -E 's/\x1b\[[0-9;]*m//g' "$1" | { grep -E '^\s+✘\s+[0-9]+ \[image\]' || [ $? -eq 1 ]; } | sed -E 's/^.*\[image\] › e2e\/([^:]+):.*/\1/' | sort | uniq -c | awk '{printf "%s%s×%s", s, $2, $1; s=", "}'
}

# run_summary <log>: Playwright's closing lines ("  20 passed (12s)", "  2 failed") on stdout, empty when the run never got that far.
# A log without them is a run that was cut off (or never started), whatever its exit code says.
run_summary() {
  sed -E 's/\x1b\[[0-9;]*m//g' "$1" | { grep -E '^\s+[0-9]+ (passed|failed)' || [ $? -eq 1 ]; }
}

# classify <name> <log> <image.sh exit code>: prints one line; returns 0 = red on a spec, 1 = green, 2 = the harness broke.
# Every command substitution is checked on its own: this runs under `classify ... || c=$?`, where bash switches errexit off for the
# whole function, so a failing stage would otherwise be shrugged off and a half-parsed log reported as RED (#460 r1).
classify() {
  local name=$1 log=$2 code=$3 failed summary st=0
  if failed=$(failed_specs "$log"); then :; else echo "mutant $name: ERROR, parsing the log failed (status $?)"; return 2; fi
  if summary=$(run_summary "$log"); then :; else echo "mutant $name: ERROR, reading the Playwright summary failed (status $?)"; return 2; fi
  echo "mutant $name: exit=$code failed-spec-files=[${failed}]"
  if [ -z "$summary" ]; then echo "mutant $name: ERROR, no Playwright summary in the log: the run was cut off or never started (image.sh exit $code)"; return 2; fi
  if [ "$code" -eq 0 ]; then return 1; fi
  if [ -z "$failed" ]; then echo "mutant $name: ERROR, exit=$code but no spec failed, the harness broke"; return 2; fi
  return 0
}

# keep_log <log> <dir> <name>: the evidence copy has to exist; a copy that fails is a failure, not a shrug.
keep_log() {
  # the log may already be where it is to be kept (no E2E_MUTANT_LOGS): cp refuses to copy a file onto itself, that is not a failure
  if [ -e "$2/$3.log" ] && [ "$1" -ef "$2/$3.log" ]; then return 0; fi
  cp "$1" "$2/$3.log"
}
