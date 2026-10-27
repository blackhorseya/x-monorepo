#!/usr/bin/env bash
# Pre-pull the images the Go tests start through testcontainers, retrying the pull.
#
#   scripts/ci-prepull-images.sh          # ci.yml's test job, before `go test`
#   DOCKER=podman scripts/ci-prepull-images.sh
#
# Overrides (scripts/test.sh uses them to run this against a fake engine): DOCKER, the engine
# command; IMAGES, a space-separated list that skips reading the names below; PULL_RETRY_SLEEP,
# the wait unit between attempts (1x, 2x, 3x this; default 5 seconds).
#
# testcontainers-go pulls a missing image while it creates the container. v0.44.0 retries its own
# ImagePull request (attemptToPullImage, with backoff), yet a Docker Hub 502 still failed a run
# (39370298272, pkgs/ordering: `reaper: ... failed to resolve reference
# "docker.io/testcontainers/ryuk:0.14.0": ... 502 Bad Gateway`); which step of the create path
# that was is not traced. With the images already local that path does not need the registry,
# and the pull here is retried by the engine CLI. Ryuk stays on: it is what removes the
# containers when a test binary is killed.
#
# The names are read from where the tests take them, so a version bump follows by itself:
#   postgres, redis  the Image constants of pgtest / redistest
#   ryuk             ReaperDefaultImage of the testcontainers-go version go.work resolves
set -euo pipefail
cd "$(dirname "$0")/.."
docker=${DOCKER:-docker}

const() { sed -n "s/^const $2 = \"\(.*\)\"\$/\1/p" "$1" | head -1; }

if [[ -n "${IMAGES:-}" ]]; then
  images=$IMAGES
else
  pg=$(const pkgs/platform/postgres/pgtest/pgtest.go Image)
  redis=$(const pkgs/platform/redis/redistest/redistest.go Image)
  (cd pkgs/platform && go mod download github.com/testcontainers/testcontainers-go)
  tc=$(cd pkgs/platform && go list -m -f '{{.Dir}}' github.com/testcontainers/testcontainers-go)
  ryuk=$(const "$tc/internal/config/config.go" ReaperDefaultImage)
  for img in "$pg" "$redis" "$ryuk"; do
    [[ -n "$img" ]] || { echo "could not read an image name (pg='$pg' redis='$redis' ryuk='$ryuk')" >&2; exit 1; }
  done
  images="$pg $redis $ryuk"
fi

# pull <image>: up to 4 attempts, 5s / 10s / 15s apart (times PULL_RETRY_SLEEP / 5).
pull() {
  local i
  for i in 1 2 3 4; do
    if "$docker" pull --quiet "$1" >/dev/null; then echo "pulled $1"; return 0; fi
    echo "pull $1 failed (attempt $i/4)" >&2
    [[ $i -lt 4 ]] && sleep $((i * ${PULL_RETRY_SLEEP:-5}))
  done
  return 1
}

# One background pull per image, each in its own process group (set -m), so that cancelling
# kills the subshell, its `sleep` and the engine it started, not just the subshell. The trap is
# in place before the first pull starts and also runs on a normal exit, where it finds nothing.
set -m
pids=""
# shellcheck disable=SC2329  # runs from the EXIT trap
cleanup() {
  trap - EXIT
  local p
  for p in $pids; do kill -TERM -- "-$p" 2>/dev/null || true; done
  for p in $pids; do wait "$p" 2>/dev/null || true; done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for img in $images; do
  pull "$img" & pids="$pids $!"
done
rc=0
for p in $pids; do wait "$p" || rc=1; done
pids=""
exit $rc
