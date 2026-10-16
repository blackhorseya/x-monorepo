#!/usr/bin/env bash
# Claude Code Stop hook. When the agent says it is done, run the local gate.
# Exit 2 blocks the stop and hands stderr back to the agent.
set -uo pipefail
cd "$(dirname "$0")/../.."

# Avoid infinite loops: if this stop was already triggered by a hook, let it through.
if python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin).get("stop_hook_active") else 1)' 2>/dev/null; then
  exit 0
fi

out=$(task gate-local 2>&1) || { echo "gate-local failed:"; echo "$out"; exit 2; } >&2
exit 0
