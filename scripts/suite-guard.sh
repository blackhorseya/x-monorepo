#!/usr/bin/env bash
# The outer gate of scripts/test.sh (#307): runs a command in a process group of its own and, once it
# ends — normally, failed, or interrupted by INT / TERM — fails if anything is still alive in that
# group, then kills it (TERM, then KILL) and waits. The suite's own EXIT trap cannot promise this: a
# fixture it forgot to register, a `set -e` abort before the registration, or a SIGKILL skips it.
#
#   scripts/suite-guard.sh <command> [args...]      # test.sh re-executes itself through this
#
# Only the group it created is touched (the command's pid is the group id, set by job control),
# never a scan of the user's processes. The command sees SUITE_GUARD=1 so it does not nest.
# Exit status: the command's, or 1 if it passed but left processes behind, or 130 / 143 on INT / TERM.
set -u
[ $# -gt 0 ] || { echo "usage: $0 <command> [args...]" >&2; exit 2; }

set -m
SUITE_GUARD=1 "$@" &
child=$!
set +m
pgid=$child
signaled=0

# stop: the signal reached the guard (the suite is in another group, so the terminal's Ctrl-C does
# not reach it); pass it on to the whole group and keep waiting for the suite to finish.
# shellcheck disable=SC2329 # runs from the traps below
stop() { signaled=$1; kill -TERM -- "-$pgid" 2>/dev/null; }
trap 'stop 130' INT
trap 'stop 143' TERM

rc=0
while :; do
  wait "$child" 2>/dev/null; rc=$?
  kill -0 "$child" 2>/dev/null || break
done

# group_alive: any member left? (the group id stays taken while a member lives, so a stale id
# cannot name somebody else's group)
group_alive() { kill -0 -- "-$pgid" 2>/dev/null; }
settle() { local i=0; while group_alive && [ $i -lt 20 ]; do sleep 0.1; i=$((i + 1)); done; }

settle   # a member that is already exiting is not a leftover
if group_alive; then
  n=$(ps -ax -o pgid= | awk -v g="$pgid" '$1 == g' | wc -l | tr -d ' ')
  echo "suite-guard: the suite left $n process(es) running in its process group ($(ps -ax -o pid=,pgid=,command= | awk -v g="$pgid" '$2 == g { print $1 ": " $3 " " $4 }' | tr '\n' ';')); killing them" >&2
  kill -TERM -- "-$pgid" 2>/dev/null
  settle
  if group_alive; then kill -KILL -- "-$pgid" 2>/dev/null; settle; fi
  group_alive && echo "suite-guard: processes of group $pgid survived KILL" >&2
  [ "$rc" != 0 ] || rc=1
fi

case $signaled in
  0) exit "$rc" ;;
  *) exit "$signaled" ;;
esac
