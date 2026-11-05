#!/usr/bin/env bash
# capture <outvar> <rcvar> <command…> — run a command, keep ALL of its output (stdout and stderr, trailing newlines trimmed like $(…)) in
# $outvar and its exit status in $rcvar. For a test that judges BOTH (#466): `out=$(cmd) || rc=$?` hides the status behind an assignment
# the checker cannot tell from a swallowed one, and `fnctx` allows only `|| return` / `|| exit` / `|| { …; return|exit; }` after a $(…) in
# a function body. capture has no $(cmd) around the command: the command's own status goes into $rcvar, and capture fails (returns 1) only
# when it cannot do its own job (no temp file, cannot read it back, bad variable name), never because the command did.
#   . scripts/capture.sh
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
