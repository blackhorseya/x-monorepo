#!/usr/bin/env bash
# best_effort <command…> — run a command whose failure CANNOT change a verdict, and say so when it fails (#462).
#   . scripts/best-effort.sh
#   best_effort kubectl -n "$ns" get pods,jobs >&2        # a diagnostic printed while failing anyway
#   best_effort kubectl -n "$ns" delete secret x --ignore-not-found   # cleanup after the verdict is decided
#   best_effort kubectl wait --for=delete pod -l app=x      # "already gone" is the state we want
# On failure it prints `best-effort command failed (exit N): <command>` to stderr and returns 0, so the failure is visible in
# the log instead of silently `|| true`. Three kinds of use only — diagnostic, cleanup, wait-gone; a command whose output or
# status is used (a poll, a lookup, a value) takes `rc=0; cmd || rc=$?` and an explicit branch instead. check-exit-swallow.sh
# refuses best_effort inside $(…) or on the right of an assignment, where its output would be used.
best_effort() {
  local rc=0
  "$@" || rc=$?
  if [ "$rc" != 0 ]; then echo "best-effort command failed (exit $rc): $*" >&2; fi
  return 0
}

# ── 還原 (restore) is NOT best effort (#462 r2) ───────────────────────────────────────────────────────────────────────
# A script that promises "the release is back the way it was when I leave" is judged by that promise: a restore that fails
# fails the gate. Use these two in the EXIT trap instead of best_effort:
#   restore() { restore_step kubectl scale … ; restore_step helm upgrade … ; }   # every step runs, each failure is printed
#   on_exit() { local rc=$?; …diagnose…; restore; restore_exit "$rc"; }; trap on_exit EXIT
# restore_exit leaves with the status the script had on the way in; when that was 0 and a restore failed it leaves with the
# first restore failure's status; when both failed it keeps the original status and says the restore failed too.
RESTORE_RC=0
restore_step() { # <command…>: run one restore command, print its failure, remember the first failing status, carry on
  local rc=0
  "$@" || rc=$?
  if [ "$rc" != 0 ]; then
    echo "restore failed (exit $rc): $*" >&2
    if [ "$RESTORE_RC" = 0 ]; then RESTORE_RC=$rc; fi
  fi
  return 0
}
restore_exit() { # <status on the way in>
  local rc=$1
  if [ "$RESTORE_RC" != 0 ]; then
    if [ "$rc" = 0 ]; then rc=$RESTORE_RC; else echo "restore also failed (exit $RESTORE_RC); keeping exit $rc" >&2; fi
  fi
  exit "$rc"
}

# ── capture / value_of (#466): fnctx's closed whitelist leaves no room for `out=$(cmd) || rc=$?` in a function body; these two are what tests and polls use instead ──
# capture [-2] <outvar> <rcvar> <command…> — run a command; its STDOUT (trailing newlines trimmed, as $(…) does) goes to $outvar and its exit status
# to $rcvar. The command's stderr is NOT captured: it goes to the caller's stderr, exactly as in `out=$(cmd)`. `-2` merges stderr into the
# output, exactly as in `out=$(cmd 2>&1)`: use it where the old code said 2>&1, and nowhere else (a warning on stderr must not become a
# parsed HTTP header). There is no $(cmd) around the command, so its status is in $rcvar whatever it printed first. capture fails (returns 1)
# only when it cannot do its own job (no temp file, cannot read it back, bad variable name), never because the command did.
#   capture out rc ./script.sh arg          # out = stdout (also after printing and THEN exiting non-zero), rc = its exit status
#   capture -2 out rc ./script.sh           # out = stdout + stderr interleaved
#   A=1 capture out rc ./script.sh          # a prefix assignment reaches the command; a `cd` needs a function with ( cd dir && cmd )
capture() {
  local _cap_merge=0 _cap_o _cap_r _cap_f _cap_rc=0 _cap_txt
  if [ "${1:-}" = -2 ]; then _cap_merge=1; shift; fi
  _cap_o=${1:-} _cap_r=${2:-}
  [ "$#" -ge 3 ] || return 1
  shift 2
  _cap_f=$(mktemp "${TMPDIR:-/tmp}/capture.XXXXXX") || return 1
  if [ "$_cap_merge" = 1 ]; then "$@" > "$_cap_f" 2>&1 || _cap_rc=$?; else "$@" > "$_cap_f" || _cap_rc=$?; fi
  _cap_txt=$(cat "$_cap_f") || return 1   # (a temp file we just wrote that cannot be read back: it is left for the OS temp cleaner, the caller gets the failure)
  rm -f "$_cap_f" || return 1
  printf -v "$_cap_o" '%s' "$_cap_txt" || return 1
  printf -v "$_cap_r" '%s' "$_cap_rc" || return 1
  return 0
}

# value_of [-ok N] <command…> — the command's stdout, when it succeeded: exit 0, or <= N with -ok N (grep -c / pgrep: 1 = nothing found).
# When it FAILED it prints the reason on stderr, prints NOTHING on stdout (not the half it wrote before it died) and returns the command's own
# exit status. A failed read is a failure of the caller, never an empty value:
#   n=$(value_of -ok 1 grep -c ready "$log") || return
# It returns 1 when it cannot do its own job (no command, no temp file).
value_of() {
  local ok=0 _vo_f _vo_rc=0
  if [ "${1:-}" = -ok ]; then ok=$2; shift 2; fi
  [ "$#" -gt 0 ] || return 1
  _vo_f=$(mktemp "${TMPDIR:-/tmp}/value_of.XXXXXX") || return 1
  "$@" > "$_vo_f" 2>/dev/null || _vo_rc=$?
  if [ "$_vo_rc" -le "$ok" ]; then
    cat "$_vo_f" || { rm -f "$_vo_f"; return 1; }
    rm -f "$_vo_f" || return 1
    return 0
  fi
  echo "value_of: command failed (exit $_vo_rc): $*" >&2
  rm -f "$_vo_f" || return 1
  return "$_vo_rc"
}
