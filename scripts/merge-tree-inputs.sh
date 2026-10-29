#!/usr/bin/env bash
# Validates merge-tree.yml's inputs before anything uses them:
#
#   scripts/merge-tree-inputs.sh <pr> <head> <base>
#
# The run title ("merge-tree pr=N head=<sha> base=<sha>") is what scripts/merge-ready.sh trusts, so
# the values in it must be exactly what the job builds: pr all digits, head and base full 40-digit
# lowercase hex — no suffix, no whitespace, no newline. Exit: 0 ok · 1 rejected · 3 usage.
set -euo pipefail
export LC_ALL=C
[ $# -eq 3 ] || { echo "usage: $0 <pr> <head> <base>" >&2; exit 3; }
pr=$1 head=$2 base=$3
[[ $pr =~ ^[0-9]+$ ]] || { echo "merge-tree-inputs: pr must be digits only" >&2; exit 1; }
[[ $head =~ ^[0-9a-f]{40}$ ]] || { echo "merge-tree-inputs: head must be a 40-digit lowercase hex SHA" >&2; exit 1; }
[[ $base =~ ^[0-9a-f]{40}$ ]] || { echo "merge-tree-inputs: base must be a 40-digit lowercase hex SHA" >&2; exit 1; }
