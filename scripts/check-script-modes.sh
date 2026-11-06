#!/usr/bin/env bash
# Gate for #454: after `task tools` runs its `chmod +x …` line, `git diff` must stay clean — i.e. every file that line
# touches is already 100755 in the git index. gen-freshness only runs `task tools` on a tools-cache miss, so a 100644 file
# in that list stays invisible while the cache hits and turns every PR red the day it is evicted. This gate does not depend
# on the cache: it reads the chmod line out of Taskfile.yml (no second copy of the list to drift), expands its globs the way
# the shell would, and compares each file's index mode. It never touches the working tree.
#
# Every external command's exit status is checked on its own: `x=$(cmd || true)` or a pipe inside $(…) would turn a tool that
# printed something and then died into a green gate. Only grep's exit 1 ("no match") is an answer; anything else aborts with 2
# (or grep/git's own code is passed on when it is not 0/1).
#
#   scripts/check-script-modes.sh      # task check-script-modes; helm-check, and CI's scripts-test job (scripts/test.sh)
set -euo pipefail
die() { echo "check-script-modes: $*" >&2; exit 2; }
_lib=${BASH_SOURCE[0]%/*}; [ "$_lib" != "${BASH_SOURCE[0]}" ] || _lib=.   # this script's own directory, whatever the cwd (parameter expansion only)
. "$_lib/best-effort.sh" || { echo "check-script-modes: cannot load $_lib/best-effort.sh (capture)" >&2; exit 2; }   # capture: the status of a command next to what it printed

here=$(dirname "$0") || { echo "check-script-modes: dirname failed" >&2; exit 2; }
cd "$here/.." || { echo "check-script-modes: cannot cd to the repo root from [$here]" >&2; exit 2; }

capture line rc grep -E '^[[:space:]]+- chmod \+x ' Taskfile.yml || exit 2
case $rc in
  0) ;;
  1) die "want exactly one '- chmod +x' line in Taskfile.yml (task tools), found none" ;;
  *) die "grep failed (exit $rc) reading Taskfile.yml" ;;
esac
case $line in *$'\n'*) die "want exactly one '- chmod +x' line in Taskfile.yml (task tools), found several: [$line]" ;; esac

# shellcheck disable=SC2086 # word splitting + globbing of the pattern list is the point
set -- ${line#*chmod +x }
bad=0 n=0
for f in "$@"; do
  if [ ! -e "$f" ]; then echo "check-script-modes: '$f' (from the chmod line) matches no file" >&2; bad=1; continue; fi
  n=$((n + 1))
  capture entry rc git ls-files -s -- "$f" || exit 2
  [ "$rc" = 0 ] || { echo "check-script-modes: git ls-files failed (exit $rc) for '$f'" >&2; exit 2; }
  mode=${entry%% *}
  [ "$mode" = 100755 ] && continue
  echo "check-script-modes: $f is [${mode:-untracked}] in the git index but 'task tools' makes it 755 — run: git update-index --chmod=+x $f" >&2
  bad=1
done
[ "$n" -ge 20 ] || { echo "check-script-modes: only $n files checked — the chmod line or its globs no longer match what they used to" >&2; exit 2; }
[ "$bad" = 0 ] || exit 1
echo "check-script-modes: $n files, all 100755 in the index"
