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
