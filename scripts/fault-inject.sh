#!/usr/bin/env bash
# Shared fault injection for the gate tests (#462). Source it:  . scripts/fault-inject.sh
#
#   inject_fail <dir> <tool> <exit> [<keep>] [<arg-glob>]    prints <dir>
#
# Writes <dir>/<tool>, a PATH wrapper that runs the real <tool> (found on the PATH as it is when inject_fail is called,
# so a stub earlier on the PATH is wrapped, not bypassed), lets its output through — all of it, or the first <keep> lines
# (<keep> = all, or a number; 0 = nothing) — and then exits <exit> whatever the real tool returned. With <arg-glob> only a
# call whose arguments ("$*") match the glob (a case pattern: * and ? work, spaces are fine, no single quote) dies; every other call is the real tool, untouched.
#   PATH="$(inject_fail "$tmp/w" grep 42):$PATH" gate.sh        # every grep prints its result, then dies
#   PATH="$(inject_fail "$tmp/w" gh 42 0 'pr checks*'):$PATH"    # `gh pr checks …` prints nothing and dies
# A gate that swallows a status (`x=$(cmd || true)`, `for i in $(seq …)`, `a | b` without pipefail, `set -- $(cmd)`) stays
# green on exactly this. The wrapper is plain sh, so it works on macOS and ubuntu alike.
inject_fail() {
  local dir=$1 tool=$2 code=$3 keep=${4:-all} glob=${5:-} real
  real=$(type -P "$tool") || { echo "inject_fail: no $tool on the PATH" >&2; return 1; }
  mkdir -p "$dir" || return 1
  {
    printf '#!/bin/sh\n'
    if [ -n "$glob" ]; then printf "g='%s'\ncase \"\$*\" in \$g) ;; *) exec \"%s\" \"\$@\" ;; esac\n" "$glob" "$real"; fi
    if [ "$keep" = all ]; then printf '"%s" "$@"\n' "$real"
    elif [ "$keep" = 0 ]; then printf '"%s" "$@" >/dev/null\n' "$real"   # BSD head has no -n 0
    else printf '"%s" "$@" | head -n %s\n' "$real" "$keep"; fi
    printf 'exit %s\n' "$code"
  } > "$dir/$tool" || return 1
  chmod +x "$dir/$tool" || return 1
  printf '%s\n' "$dir"
}
