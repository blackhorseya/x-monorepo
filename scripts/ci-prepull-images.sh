#!/usr/bin/env bash
# Pre-pull the images the Go tests start through testcontainers, retrying the pull.
#
#   scripts/ci-prepull-images.sh          # ci.yml's test job, before `go test`
#   DOCKER=podman scripts/ci-prepull-images.sh
#
# testcontainers-go creates each container and lets the daemon pull a missing image inside that
# call, with no retry: one 502 from Docker Hub fails the test (run 39370298272, pkgs/ordering:
# `reaper: ... failed to resolve reference "docker.io/testcontainers/ryuk:0.14.0": ... 502 Bad
# Gateway`). With the images already local that call never touches the registry, and the pull
# here is retried. Ryuk stays on: it is what removes the containers when a test binary is killed.
#
# The names are read from where the tests take them, so a version bump follows by itself:
#   postgres, redis  the Image constants of pgtest / redistest
#   ryuk             ReaperDefaultImage of the testcontainers-go version go.work resolves
set -euo pipefail
cd "$(dirname "$0")/.."
docker=${DOCKER:-docker}

const() { sed -n "s/^const $2 = \"\(.*\)\"\$/\1/p" "$1" | head -1; }

pg=$(const pkgs/platform/postgres/pgtest/pgtest.go Image)
redis=$(const pkgs/platform/redis/redistest/redistest.go Image)
(cd pkgs/platform && go mod download github.com/testcontainers/testcontainers-go)
tc=$(cd pkgs/platform && go list -m -f '{{.Dir}}' github.com/testcontainers/testcontainers-go)
ryuk=$(const "$tc/internal/config/config.go" ReaperDefaultImage)

for img in "$pg" "$redis" "$ryuk"; do
  [[ -n "$img" ]] || { echo "could not read an image name (pg='$pg' redis='$redis' ryuk='$ryuk')" >&2; exit 1; }
done

# pull <image>: up to 4 attempts, 5s / 10s / 15s apart.
pull() {
  local i
  for i in 1 2 3 4; do
    if "$docker" pull --quiet "$1" >/dev/null; then echo "pulled $1"; return 0; fi
    echo "pull $1 failed (attempt $i/4)" >&2
    [[ $i -lt 4 ]] && sleep $((i * 5))
  done
  return 1
}

pids=""
for img in "$pg" "$redis" "$ryuk"; do
  pull "$img" & pids="$pids $!"
done
rc=0
for p in $pids; do wait "$p" || rc=1; done
exit $rc
