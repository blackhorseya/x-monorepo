#!/usr/bin/env bash
# RED stub (#478): today's behaviour of dependabot-sync.yml — the sync command runs once, whatever it left behind.
set -euo pipefail
bash -c "${SYNC_FIXPOINT_CMD:-task tidy}"
