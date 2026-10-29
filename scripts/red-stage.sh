#!/usr/bin/env bash
# Where did a RED test run fail? Reads `go test` output on stdin and prints one class:
#
#   assertion  --- FAIL, and the expected message was printed (-m) → the target behaviour is reached
#   setup      --- FAIL, but the expected message never appeared → fixture / upstream rejected first
#   compile    [build failed] / [setup failed] → only proves a missing or broken API, not a behaviour
#   panic      panic: before any assertion
#   pass       nothing failed → not a RED at all
#
#   go test -run '^TestX$' ./pkg/... 2>&1 | scripts/red-stage.sh -m 'want 1 call'
#
# Exit 0 only for `assertion`; 64 when -m is missing. It is a candidate classifier: matching the
# message proves the assertion text was printed, not that it is the right assertion, so the
# reviewer still reads the test (a compile failure from an unrelated typo also says `compile`).
# Go prints test logs indented and `=== RUN` / `--- FAIL` unindented, so those lines are not
# searched for the message; -m is an ERE.
set -euo pipefail
msg=
while getopts m: o; do
  case $o in m) msg=$OPTARG ;; *) exit 64 ;; esac
done
[ -n "$msg" ] || { echo "usage: red-stage.sh -m <expected assertion message regex> < go-test-output" >&2; exit 64; }

out=$(cat)
class=pass
if grep -Eq '\[(build|setup) failed\]' <<<"$out"; then class=compile
elif grep -Eq '^panic: ' <<<"$out"; then class=panic
elif grep -Eq '^--- FAIL: ' <<<"$out"; then
  if grep -Ev '^(=== |--- |FAIL|ok )' <<<"$out" | grep -Eq -- "$msg"; then class=assertion; else class=setup; fi
fi
echo "$class"
[ "$class" = assertion ]
