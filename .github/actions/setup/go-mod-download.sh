#!/usr/bin/env bash
# Fill the module cache for the whole go.work workspace, retrying the download (#450).
#
#   .github/actions/setup/go-mod-download.sh
#
# Only the download is retried, as a whole: a failure of any kind is retried, and after the last
# attempt the script exits with that attempt's own exit code. The log carries a fixed message and
# the exit code, never the text go printed (it comes from the module proxy). Overrides:
# GO_MOD_DOWNLOAD_ATTEMPTS (default 3), GO_MOD_DOWNLOAD_SLEEP, the wait unit
# between attempts (1x, 2x, ...; default 2 seconds).
#
# Why: a cold module cache made `test (apps/migrate)` fail before any test ran, on an HTTP/2
# INTERNAL_ERROR from the proxy while fetching go.opentelemetry.io/otel (#459).
set -euo pipefail

attempts=${GO_MOD_DOWNLOAD_ATTEMPTS:-3}
unit=${GO_MOD_DOWNLOAD_SLEEP:-2}
rc=0
for ((i = 1; i <= attempts; i++)); do
  rc=0
  go mod download >/dev/null 2>&1 || rc=$?
  if [[ $rc -eq 0 ]]; then
    echo "go mod download: ok (attempt $i/$attempts)"
    exit 0
  fi
  echo "go mod download: failed with exit code $rc (attempt $i/$attempts)" >&2
  if [[ $i -lt $attempts ]]; then sleep $((i * unit)); fi
done
echo "go mod download: giving up after $attempts attempts, last exit code $rc" >&2
exit "$rc"
