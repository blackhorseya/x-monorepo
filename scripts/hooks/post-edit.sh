#!/usr/bin/env bash
# Claude Code PostToolUse hook (Edit|Write). Reads the tool input from stdin,
# runs the fast gate for the touched file's module. Exit 2 = feed stderr back
# to the agent so it fixes the problem itself.
set -uo pipefail
cd "$(dirname "$0")/../.."

file=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("tool_input",{}).get("file_path",""))' 2>/dev/null || true)
[[ -z "$file" ]] && exit 0
rel=${file#"$PWD/"}

case "$rel" in
  *.go|*.proto|go.mod|go.work|buf.yaml|buf.gen*.yaml) ;;
  *) exit 0 ;;
esac

out=$(task -s gate-fast FILE="$rel" 2>&1) || { echo "$out" >&2; exit 2; }
exit 0
