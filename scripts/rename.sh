#!/usr/bin/env bash
# Rewrite the Go module path prefix everywhere it is spelled out, for repos created
# from this template:
#
#   scripts/rename.sh github.com/you/your-repo     # (or: task rename -- github.com/you/your-repo)
#
# The current prefix is derived from pkgs/kernel/go.mod, so this is idempotent and
# works again after a second rename. Generated code (*.pb.go, *.connect.go,
# wire_gen.go) is skipped on purpose: protobuf descriptors are length-prefixed, so a
# textual replace would corrupt them — regenerate with `task gen wire` afterwards
# (`task rename` does that for you).
set -euo pipefail
cd "$(dirname "$0")/.."

NEW="${1:-}"
[[ -n "$NEW" ]] || { echo "usage: $0 <new module prefix, e.g. github.com/you/repo>" >&2; exit 2; }
[[ "$NEW" =~ ^[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)+$ ]] || { echo "not a module path: $NEW" >&2; exit 2; }

OLD=$(go mod edit -print pkgs/kernel/go.mod | awk '/^module /{print $2}')
OLD=${OLD%/pkgs/kernel}
[[ "$OLD" != "$NEW" ]] || { echo "module prefix is already $NEW"; exit 0; }

echo "$OLD → $NEW"
git ls-files -z \
  | grep -zvE '\.pb\.go$|\.connect\.go$|wire_gen\.go$|^LICENSE$' \
  | xargs -0 grep -l --null -- "$OLD" \
  | xargs -0 perl -pi -e 's{\Q'"$OLD"'\E}{'"$NEW"'}g'

# README title follows the repo name.
perl -pi -e 's{^# \Q'"${OLD##*/}"'\E$}{# '"${NEW##*/}"'}' README.md

git --no-pager diff --stat | tail -1
echo "next: task gen wire tidy   (task rename runs these for you)"
