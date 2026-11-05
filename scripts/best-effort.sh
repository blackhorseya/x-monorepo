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
