#!/usr/bin/env bash
# Gate for #454: after `task tools` runs its `chmod +x …` line, `git diff` must stay clean — i.e. every file that line
# touches is already 100755 in the git index. gen-freshness only runs `task tools` on a tools-cache miss, so a 100644 file
# in that list stays invisible while the cache hits and turns every PR red the day it is evicted. This gate does not depend
# on the cache: it reads the chmod line out of Taskfile.yml (no second copy of the list to drift), expands its globs the way
# the shell would, and compares each file's index mode. It never touches the working tree.
#
#   scripts/check-script-modes.sh      # task check-script-modes; CI's scripts-test job (scripts/test.sh) and gen-freshness
set -euo pipefail
cd "$(dirname "$0")/.."

line=$(grep -E '^[[:space:]]+- chmod \+x ' Taskfile.yml || true)
[ "$(printf '%s\n' "$line" | grep -c .)" = 1 ] || { echo "check-script-modes: want exactly one '- chmod +x' line in Taskfile.yml (task tools), found: [$line]" >&2; exit 2; }

# shellcheck disable=SC2086 # word splitting + globbing of the pattern list is the point
set -- ${line#*chmod +x }
bad=0 n=0
for f in "$@"; do
  if [ ! -e "$f" ]; then echo "check-script-modes: '$f' (from the chmod line) matches no file" >&2; bad=1; continue; fi
  n=$((n + 1))
  mode=$(git ls-files -s -- "$f" | cut -d' ' -f1)
  [ "$mode" = 100755 ] && continue
  echo "check-script-modes: $f is [${mode:-untracked}] in the git index but 'task tools' makes it 755 — run: git update-index --chmod=+x $f" >&2
  bad=1
done
[ "$n" -ge 20 ] || { echo "check-script-modes: only $n files checked — the chmod line or its globs no longer match what they used to" >&2; exit 2; }
[ "$bad" = 0 ] || exit 1
echo "check-script-modes: $n files, all 100755 in the index"
