#!/usr/bin/env bash
# Lists the files changed between two commits that end up in a shipped artifact (the goreleaser
# archives, the ghcr images, or the config they carry). release.yml's prepare job skips the whole
# release when this prints nothing; run it locally to see whether a change will cut a version:
#
#   scripts/releasable.sh [base] [head]      # default: last v* tag reachable from HEAD .. HEAD
#   task release-gate                        # same thing
#
# Everything is releasable unless excluded here. Excluded = changes no shipped artifact:
# docs, the web console, CI / hook / lint / task configuration, tests and test-only helpers,
# the Helm chart (not published anywhere), the kind / compose dev environments. The Dockerfile,
# configs/config.example.yaml (packed into the archives) and .goreleaser.yaml stay releasable.
# README.md / LICENSE are packed too, but a stale copy there is acceptable.
# Pathspec `*` crosses `/`, so `*.md` is any depth.
set -euo pipefail
head=${2:-HEAD}
base=${1:-$(git describe --tags --abbrev=0 --match 'v*' "$head" 2>/dev/null || true)}
if [ -z "$base" ]; then
  echo "releasable: no previous v* tag reachable from $head" >&2
  exit 2
fi
git diff --name-only "$base" "$head" -- . \
  ':!*.md' ':!docs' ':!web' ':!.github' ':!.claude' ':!.idea' ':!.gitignore' \
  ':!Taskfile.yml' ':!lefthook.yml' ':!.golangci.yml' ':!scripts' ':!buf.gen.ts.yaml' \
  ':!deployments/helm' ':!deployments/docker/docker-compose.yaml' ':!deployments/docker/grafana' \
  ':!*_test.go' ':!testdata' ':!tests' ':!pkgs/platform/archtest' ':!pkgs/platform/postgres/pgtest'
