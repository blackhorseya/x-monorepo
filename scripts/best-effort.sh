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

# ── capture / value_or_empty (#466): fnctx's closed whitelist leaves no room for `out=$(cmd) || rc=$?` in a function body; these two are what tests and polls use instead ──
# capture <outvar> <rcvar> <command…> — run a command, keep ALL of its output (stdout and stderr, trailing newlines trimmed like $(…)) in
# $outvar and its exit status in $rcvar. For a test that judges BOTH (#466): `out=$(cmd) || rc=$?` hides the status behind an assignment
# the checker cannot tell from a swallowed one, and `fnctx` allows only `|| return` / `|| exit` / `|| { …; return|exit; }` after a $(…) in
# a function body. capture has no $(cmd) around the command: the command's own status goes into $rcvar, and capture fails (returns 1) only
# when it cannot do its own job (no temp file, cannot read it back, bad variable name), never because the command did.
#   . scripts/best-effort.sh
#   capture out rc ./script.sh arg          # out = what it printed (also after printing and THEN exiting non-zero), rc = its exit status
#   A=1 capture out rc ./script.sh          # a prefix assignment reaches the command; a `cd` needs a function with ( cd dir && cmd )
capture() {
  local _cap_o=$1 _cap_r=$2 _cap_f _cap_rc=0 _cap_txt
  shift 2
  [ "$#" -gt 0 ] || return 1
  _cap_f=$(mktemp "${TMPDIR:-/tmp}/capture.XXXXXX") || return 1
  "$@" > "$_cap_f" 2>&1 || _cap_rc=$?
  _cap_txt=$(cat "$_cap_f") || { rm -f "$_cap_f"; return 1; }
  rm -f "$_cap_f" || return 1
  printf -v "$_cap_o" '%s' "$_cap_txt" || return 1
  printf -v "$_cap_r" '%s' "$_cap_rc" || return 1
  return 0
}

# value_or_empty [-ok N] <command…> — print the command's stdout when it succeeds (exit 0, or <= N with -ok N: grep's 1 = nothing found);
# when it fails, say so on stderr and print NOTHING — not the half of the output it produced before it died. Always returns 0 on a
# failed command (the caller polls, or treats an empty value as "not there"); returns 1 only when it cannot do its own job.
#   n=$(value_or_empty -ok 1 grep -c ready "$log") || return      # "" when grep broke, the count (0 too) otherwise
value_or_empty() {
  local ok=0 _voe_f _voe_rc=0
  if [ "${1:-}" = -ok ]; then ok=$2; shift 2; fi
  [ "$#" -gt 0 ] || return 1
  _voe_f=$(mktemp "${TMPDIR:-/tmp}/voe.XXXXXX") || return 1
  "$@" > "$_voe_f" 2>/dev/null || _voe_rc=$?
  if [ "$_voe_rc" -le "$ok" ]; then
    cat "$_voe_f" || { rm -f "$_voe_f"; return 1; }
  else
    echo "value_or_empty: command failed (exit $_voe_rc): $*" >&2
  fi
  rm -f "$_voe_f" || return 1
  return 0
}
