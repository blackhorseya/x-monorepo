#!/usr/bin/env bash
# Generate two sample BCs (one plain, one with --events) in a COPY of the working tree and prove they lint, build, test and pass their own
# acceptance (#518 slice 4, #615).
# Nothing in the repository is written. The copy is `git ls-files -co --exclude-standard`: tracked and untracked-not-ignored files, so local
# uncommitted work (and a new file you have not added yet) is included; ignored files (./bin, node_modules, generated web output) are not.
#
#   scripts/new-bc-check.sh      # needs the tools of `task tools` in ./bin (buf, protoc-gen-*, wire, golangci-lint), go, jq, git
#
# The acceptance step starts a Postgres container (pgtest, so Docker or Podman) and builds apps/migrate and apps/worker: it runs the generated
# features, the event scenario included, against the real rpc and a worker subprocess. Not proven here: the BCs' postgres repository tests
# (`go test` in pkgs/<bc> has no database); gen-ts is not run (needs network, not part of a sample).
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d) || exit 1
cleanup() {
  case "$tmp" in
    */tmp.* | */tmp/*) rm -rf "$tmp" ;;
    *) echo "new-bc-check: refusing to delete [$tmp]" >&2 ;;
  esac
}
trap cleanup EXIT INT TERM

step() { printf '\n== new-bc-check: %s\n' "$*"; }
cd "$repo"
step "copy the working tree to $tmp"
git ls-files -z -co --exclude-standard | while IFS= read -r -d '' f; do
  [ -e "$f" ] || continue # tracked but deleted in the working tree
  printf '%s\0' "$f"
done | rsync -a --from0 --files-from=- ./ "$tmp/"
ln -s "$repo/bin" "$tmp/bin" # the pinned tools: building them again would be the slow part

cd "$tmp"
# The real entry, `task new-bc` (generate, then gen, tidy to a fixed point, wire), once per BC: running its steps by hand in another order is how #602
# stayed green (a separate `task gen` re-reads go.work, `task new-bc` did not). Each run must leave the new BC's openapi.yaml: apps/worker embeds it.
new_bc() {
  local bc=$1
  step "task new-bc -- $*"
  task new-bc -- "$@"
  if [ ! -f "pkgs/$bc/contract/v1/openapi.yaml" ]; then
    echo "new-bc-check: task new-bc -- $* did not write pkgs/$bc/contract/v1/openapi.yaml (#602)" >&2
    exit 1
  fi
}
new_bc cisample
new_bc cisampleevents --events

for bc in cisample cisampleevents; do
  step "pkgs/$bc: vet, lint, test"
  (cd "pkgs/$bc" && go vet ./... && "$repo/bin/golangci-lint" run ./... && go test -count=1 -shuffle=on ./...)
done
step "domain coverage gate: every generated domain package must be at 100% (scripts/cover-gate.sh)"
COVER_PROFILE_DIR="$tmp/cover-profiles" scripts/cover-gate.sh # not ./bin: that is a symlink to the repo's bin, and parallel runs would share the profiles
step "pkgs/platform: the repo-wide module guards see the new BCs (every module carries the pgtest cleanup gate)"
(cd pkgs/platform && go test -count=1 -run '^TestTestCleanup_' ./archtest/)
for app in rpc worker migrate; do
  step "apps/$app: build, vet, architecture"
  (cd "apps/$app" && go build ./... && go vet ./... && go test -count=1 -run '^TestArchitecture$' ./...)
done
# pkgs/<bc>/acceptance imports godog and the apps' tests import it: no shipped binary may link it (only non-test dependencies are listed).
step "apps/rpc, worker, migrate: no binary links godog through the new BCs' acceptance packages"
for app in rpc worker migrate; do
  if deps=$(cd "apps/$app" && go list -deps . | grep -E '^github.com/cucumber/'); then
    echo "new-bc-check: apps/$app links $(echo "$deps" | tr '\n' ' '): the acceptance package of a BC must be imported by test files only" >&2
    exit 1
  fi
done
# The generated acceptance, through the real runner of the copy: apps/rpc's TestAcceptance finds the BCs in go.work, the registrations
# `task new-bc` added at the new-bc:acceptance anchor, and each pkgs/<bc>/acceptance/features. It fails by itself when a BC is not registered,
# has no scenario, or fewer scenarios ran than the feature files hold; the greps below make the sample BCs' own features visible in the log.
step "apps/rpc: TestAcceptance runs the platform's and both sample BCs' features (Postgres container, migrate, worker)"
log="$tmp/acceptance.log"
(cd apps/rpc && go test -count=1 -v -run '^TestAcceptance$' ./) 2>&1 | tee "$log"
plain=$(LC_ALL=C sed -e $'s/\033\\[[0-9;]*m//g' "$log")
for want in "cisample/acceptance/features/cisample.feature" "cisampleevents/acceptance/features/cisampleevents.feature" "the worker consumed the event after"; do
  case "$plain" in
    *"$want"*) ;;
    *) echo "new-bc-check: the acceptance output has no [$want]: the sample BC's feature did not run" >&2; exit 1 ;;
  esac
done
printf '%s\n' "$plain" | grep -E '^[0-9]+ scenarios? \(|^[0-9]+ steps? \(|the worker consumed the event after'
step "ok"
