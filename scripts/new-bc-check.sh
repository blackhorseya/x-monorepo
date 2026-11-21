#!/usr/bin/env bash
# Generate two sample BCs (one plain, one with --events) in a COPY of the working tree and prove they lint, build and test (#518 slice 4).
# Nothing in the repository is written. The copy is `git ls-files -co --exclude-standard`: tracked and untracked-not-ignored files, so local
# uncommitted work (and a new file you have not added yet) is included; ignored files (./bin, node_modules, generated web output) are not.
#
#   scripts/new-bc-check.sh      # needs the tools of `task tools` in ./bin (buf, protoc-gen-*, wire, golangci-lint), go, jq, git
#
# Not proven here: the postgres repository tests (no database in the gen-freshness job). gen-ts is not run (needs network, not part of a sample).
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
step "ok"
