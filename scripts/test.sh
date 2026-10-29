#!/usr/bin/env bash
# Tests for the release / CI decision scripts: scripts/ci-changes.sh (which ci.yml jobs a change
# runs), scripts/releasable.sh (does a change cut a release), scripts/next-version.sh (which
# version) and scripts/release-gate-pr.sh (the PR comment built from the three). A wrong rule in
# ci-changes.sh skips a job silently — CI stays green — so the rules are pinned here.
#
#   scripts/test.sh          # task scripts-test; CI's scripts-test job
#
# Each case builds a throwaway git repo with scripts/ copied in and runs the copies there
# (ci-changes.sh cd's to its own repo root; release-gate-pr.sh calls scripts/ relative to cwd).
# Plain bash 3.2 (macOS) and 5 (ubuntu): no mapfile, no associative arrays.
set -euo pipefail
src=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
# The fixtures must not inherit the developer's signing / hook settings.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
trap 'rm -rf "$tmp"' EXIT
fail=0 pass=0

# fixture <name>: a fresh repo at $repo with scripts/ committed on main and tag v0.1.0.
fixture() {
  repo="$tmp/$1"
  mkdir -p "$repo"
  git -C "$repo" init -q -b main
  cp -R "$src/scripts" "$repo/scripts"
  echo base > "$repo/README.md"
  commit "chore: base"
  git -C "$repo" tag v0.1.0
  # ci-changes.sh diffs against origin/main: point it at this repo's main.
  git -C "$repo" update-ref refs/remotes/origin/main main
}
# touch_file <path> [content]
touch_file() { mkdir -p "$repo/$(dirname "$1")"; echo "${2:-x}" > "$repo/$1"; }
# commit "<subject>" ["<body>"]: stage everything and commit.
commit() {
  git -C "$repo" add -A
  if [ -n "${2:-}" ]; then git -C "$repo" commit -q --allow-empty -m "$1" -m "$2"; else git -C "$repo" commit -q --allow-empty -m "$1"; fi
}
in_repo() { (cd "$repo" && "$@"); }

ok()   { pass=$((pass + 1)); }
bad()  { fail=$((fail + 1)); echo "FAIL: $*" >&2; }
# expect_eq <name> <want> <got>
expect_eq() { if [ "$2" = "$3" ]; then ok; else bad "$1: want [$2] got [$3]"; fi; }
# expect_match <name> <regex> <text>
expect_match() { if printf '%s' "$3" | grep -Eq -- "$2"; then ok; else bad "$1: [$3] !~ /$2/"; fi; }

# ── ci-changes.sh ───────────────────────────────────────────────────────────────
all_jobs="gen_freshness lint work_sync coverage build breaking release_snapshot actions_resolve web console_e2e console_image helm scripts_test helm_kind k6"
# jobs_on <name> <files...>: commits the files on a branch off main and returns the jobs set to true.
jobs_on() {
  local name=$1; shift
  fixture "$name"
  git -C "$repo" checkout -q -b change
  for f in "$@"; do touch_file "$f"; done
  commit "chore: $name"
  in_repo scripts/ci-changes.sh | sed -n 's/^run_\(.*\)=true$/\1/p' | tr '\n' ' ' | sed 's/ $//'
}
expect_eq "docs only runs nothing"            "" "$(jobs_on docs README.md docs/a.md deployments/docker/grafana/x.json)"
expect_eq "go runs the Go jobs, kind and k6" \
  "gen_freshness lint work_sync coverage build release_snapshot console_e2e console_image helm_kind k6" \
  "$(jobs_on go pkgs/ordering/domain/order/order.go)"
expect_eq "go.mod counts as go" \
  "gen_freshness lint work_sync coverage build release_snapshot console_e2e console_image helm_kind k6" \
  "$(jobs_on gomod pkgs/ordering/go.mod)"
expect_eq "proto is go + breaking + web" \
  "gen_freshness lint work_sync coverage build breaking release_snapshot web console_e2e console_image helm_kind k6" \
  "$(jobs_on proto protos/ordering/v1/service.proto)"
expect_eq "web only: no lint, no k6" \
  "gen_freshness web console_e2e console_image helm_kind" \
  "$(jobs_on web web/apps/console/src/routes/orders.tsx)"
expect_eq "helm only" "helm helm_kind" "$(jobs_on helm deployments/helm/x-monorepo/values.yaml)"
expect_eq "k6 only" "k6" "$(jobs_on k6 tests/k6/orders.js)"
expect_eq "Dockerfile is an image change" \
  "release_snapshot console_image helm_kind" \
  "$(jobs_on image deployments/docker/Dockerfile.rpc)"
expect_eq ".goreleaser.yaml is an image change" \
  "release_snapshot console_image helm_kind" \
  "$(jobs_on goreleaser .goreleaser.yaml)"
expect_eq "another workflow is actions-resolve only" "actions_resolve" "$(jobs_on wf .github/workflows/release.yml)"
expect_eq "ci.yml runs everything"   "$all_jobs" "$(jobs_on ciyml .github/workflows/ci.yml)"
expect_eq "Taskfile runs everything" "$all_jobs" "$(jobs_on taskfile Taskfile.yml)"
expect_eq "scripts/ runs everything" "$all_jobs" "$(jobs_on scripts scripts/new.sh)"
expect_eq "configs/ is go" \
  "gen_freshness lint work_sync coverage build release_snapshot console_e2e console_image helm_kind k6" \
  "$(jobs_on configs configs/config.example.yaml)"

fixture ciall
expect_eq "CI_ALL=1 runs everything" "$all_jobs" "$(in_repo env CI_ALL=1 scripts/ci-changes.sh | sed -n 's/^run_\(.*\)=true$/\1/p' | tr '\n' ' ' | sed 's/ $//')"
expect_eq "CI_ALL prints one flag per job" "$(echo "$all_jobs" | wc -w | tr -d ' ')" "$(in_repo env CI_ALL=1 scripts/ci-changes.sh | wc -l | tr -d ' ')"

# A branch behind main must see only its own files (three-dot diff), not main's newer ones.
fixture behind
git -C "$repo" checkout -q -b change
touch_file tests/k6/x.js; commit "test: k6"
git -C "$repo" checkout -q main
touch_file pkgs/x/a.go; commit "feat: go on main"
git -C "$repo" update-ref refs/remotes/origin/main main
git -C "$repo" checkout -q change
expect_eq "branch behind main sees only its own files" "k6" "$(in_repo scripts/ci-changes.sh | sed -n 's/^run_\(.*\)=true$/\1/p' | tr '\n' ' ' | sed 's/ $//')"

# ── affected.sh ─────────────────────────────────────────────────────────────────
# The test matrix runs the modules this prints. A file no module claims must still run all of them
# when it is something the test job runs on (ci.yml, .github/actions, scripts/), and --all (main,
# dispatch) must list every module whatever the diff is.
# affected_fixture <name>: $repo with go.work over pkgs/a and pkgs/b (b requires a), committed on main.
affected_fixture() {
  fixture "$1"
  mkdir -p "$repo/pkgs/a" "$repo/pkgs/b"
  printf 'module example.com/a\n\ngo 1.22\n' > "$repo/pkgs/a/go.mod"
  printf 'module example.com/b\n\ngo 1.22\n\nrequire example.com/a v0.0.0\n' > "$repo/pkgs/b/go.mod"
  printf 'go 1.22\n\nuse (\n\t./pkgs/a\n\t./pkgs/b\n)\n' > "$repo/go.work"
  commit "chore: modules"
  git -C "$repo" update-ref refs/remotes/origin/main main
}
# affected_on <name> <files...>: commits the files on a branch off main and prints the modules.
affected_on() {
  local name=$1; shift
  affected_fixture "$name"
  git -C "$repo" checkout -q -b change
  for f in "$@"; do touch_file "$f"; done
  commit "chore: $name"
  in_repo scripts/affected.sh | tr '\n' ' ' | sed 's/ $//'
}
expect_eq "affected: docs touch no module"           ""              "$(affected_on adocs README.md docs/a.md)"
expect_eq "affected: a file under a module"          "pkgs/b"        "$(affected_on amod pkgs/b/x.go)"
expect_eq "affected: dependents follow"              "pkgs/a pkgs/b" "$(affected_on adep pkgs/a/x.go)"
expect_eq "affected: ci.yml is every module"         "pkgs/a pkgs/b" "$(affected_on aci .github/workflows/ci.yml)"
expect_eq "affected: .github/actions is every module" "pkgs/a pkgs/b" "$(affected_on aact .github/actions/setup/action.yml)"
expect_eq "affected: scripts/ is every module"       "pkgs/a pkgs/b" "$(affected_on ascripts scripts/new.sh)"
expect_eq "affected: another workflow is no module"  ""              "$(affected_on awf .github/workflows/release.yml)"
affected_fixture aall
expect_eq "affected: --all ignores the diff"         "pkgs/a pkgs/b" "$(in_repo scripts/affected.sh --all | tr '\n' ' ' | sed 's/ $//')"

# ── ci-prepull-images.sh ────────────────────────────────────────────────────────
# Run against a fake engine (DOCKER=...) with the image list and the backoff overridden. The fake
# logs each pull to $FAKE_LOG and either succeeds after FAKE_FAIL_FIRST failures per image or, in
# hang mode, records its pid and blocks like an engine stuck on a registry.
prepull_tmp=$(mktemp -d)
fake_engine="$prepull_tmp/engine"
# The hang case below starts the script in the background. If the suite is cancelled meanwhile,
# stop that script (its own trap takes the fake engines down) and wait, so nothing outlives us.
prepull_pid=""
prepull_stop() {
  [ -n "$prepull_pid" ] || return 0
  kill -TERM "$prepull_pid" 2>/dev/null || true
  wait "$prepull_pid" 2>/dev/null || true
  prepull_pid=""
}
trap 'prepull_stop; rm -rf "$tmp" "$prepull_tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cat > "$fake_engine" <<'ENGINE'
#!/usr/bin/env bash
# usage: engine pull --quiet <image>
img=$3
echo "$img" >> "$FAKE_LOG"
if [ "${FAKE_HANG:-}" = 1 ]; then echo $$ >> "$FAKE_PIDS"; exec sleep 600; fi
n=$(grep -cx "$img" "$FAKE_LOG")
[ "$n" -gt "${FAKE_FAIL_FIRST:-0}" ]
ENGINE
chmod +x "$fake_engine"
prepull() { # prepull <fail-first> -> prints "<exit> <attempts of a> <b> <c>"
  : > "$prepull_tmp/log"
  local rc=0
  FAKE_LOG="$prepull_tmp/log" FAKE_FAIL_FIRST=$1 DOCKER="$fake_engine" IMAGES="a b c" PULL_RETRY_SLEEP=0 \
    "$src/scripts/ci-prepull-images.sh" >/dev/null 2>&1 || rc=$?
  echo "$rc $(grep -cx a "$prepull_tmp/log") $(grep -cx b "$prepull_tmp/log") $(grep -cx c "$prepull_tmp/log")"
}
expect_eq "prepull: all succeed at once"             "0 1 1 1" "$(prepull 0)"
expect_eq "prepull: two failures then success"       "0 3 3 3" "$(prepull 2)"
expect_eq "prepull: gives up after four attempts"    "1 4 4 4" "$(prepull 9)"

# Cancelled while the pulls hang: no engine process may outlive the script.
: > "$prepull_tmp/log"; : > "$prepull_tmp/pids"
FAKE_HANG=1 FAKE_LOG="$prepull_tmp/log" FAKE_PIDS="$prepull_tmp/pids" DOCKER="$fake_engine" IMAGES="a b c" PULL_RETRY_SLEEP=0 \
  "$src/scripts/ci-prepull-images.sh" >/dev/null 2>&1 &
prepull_pid=$!
for _ in $(seq 50); do [ "$(wc -l < "$prepull_tmp/pids" | tr -d ' ')" = 3 ] && break; sleep 0.1; done
expect_eq "prepull: three engines started" "3" "$(wc -l < "$prepull_tmp/pids" | tr -d ' ')"
prepull_stop
sleep 0.5
alive=0
for p in $(cat "$prepull_tmp/pids"); do
  if kill -0 "$p" 2>/dev/null; then alive=$((alive + 1)); kill -KILL "$p" 2>/dev/null || true; fi
done
expect_eq "prepull: SIGTERM leaves no engine running" "0" "$alive"
rm -rf "$prepull_tmp"

# ── releasable.sh ───────────────────────────────────────────────────────────────
fixture releasable
touch_file README.md changed; touch_file docs/guide.md; touch_file pkgs/ordering/README.md
touch_file deployments/helm/x-monorepo/Chart.yaml; touch_file web/apps/console/e2e/a.spec.ts
touch_file pkgs/ordering/domain/order/order_test.go; touch_file .github/workflows/ci.yml
touch_file scripts/x.sh; touch_file Taskfile.yml; touch_file tests/k6/a.js
touch_file deployments/docker/docker-compose.yaml; touch_file deployments/docker/grafana/d.json
commit "docs: nothing shipped"
expect_eq "docs / chart / tests / CI are not releasable" "" "$(in_repo scripts/releasable.sh)"
touch_file pkgs/ordering/domain/order/order.go; touch_file deployments/docker/Dockerfile.rpc
touch_file configs/config.example.yaml; touch_file web/apps/console/src/main.tsx; touch_file .goreleaser.yaml
commit "feat: shipped"
expect_eq "shipped files are releasable" \
  ".goreleaser.yaml
configs/config.example.yaml
deployments/docker/Dockerfile.rpc
pkgs/ordering/domain/order/order.go
web/apps/console/src/main.tsx" \
  "$(in_repo scripts/releasable.sh)"
expect_eq "explicit range sees only that range" "" "$(in_repo scripts/releasable.sh HEAD~2 HEAD~1)"
fixture notag
git -C "$repo" tag -d v0.1.0 >/dev/null
set +e; in_repo scripts/releasable.sh >/dev/null 2>&1; rc=$?; set -e
expect_eq "no tag exits 2" 2 "$rc"

# ── next-version.sh ─────────────────────────────────────────────────────────────
nv() { in_repo scripts/next-version.sh "$@" | cut -d' ' -f1; }
fixture nv
expect_eq "no commits: patch" v0.1.1 "$(nv)"
commit "fix: a"; commit "docs: b"
expect_eq "fix + docs: patch" v0.1.1 "$(nv)"
commit "feat(console): c"
expect_eq "feat: minor" v0.2.0 "$(nv)"
expect_eq "feat: reason names it" "v0.2.0 (minor — feat: feat(console): c; base v0.1.0)" "$(in_repo scripts/next-version.sh)"
commit "refactor(rpc)!: d"
expect_eq "!: major" v1.0.0 "$(nv)"
fixture nvbody
commit "fix: a" "BREAKING CHANGE: the wire changed"
expect_eq "BREAKING CHANGE body: major" v1.0.0 "$(nv)"
fixture nvtitle
commit "fix: a"
expect_eq "title arg bumps patch to minor" v0.2.0 "$(nv v0.1.0 HEAD "feat: pending PR")"
expect_eq "title arg bumps to major"       v1.0.0 "$(nv v0.1.0 HEAD "feat!: pending PR")"
expect_eq "title arg leaves patch alone"   v0.1.1 "$(nv v0.1.0 HEAD "docs: pending PR")"
commit "feat!: b"
expect_eq "title never lowers major"       v1.0.0 "$(nv v0.1.0 HEAD "fix: pending PR")"
fixture nvnotag
git -C "$repo" tag -d v0.1.0 >/dev/null
set +e; in_repo scripts/next-version.sh >/dev/null 2>&1; rc=$?; set -e
expect_eq "next-version without tag exits 2" 2 "$rc"

# ── release-gate-pr.sh ──────────────────────────────────────────────────────────
fixture gate
git -C "$repo" checkout -q -b pr
touch_file README.md changed; commit "docs: readme"
out=$(in_repo scripts/release-gate-pr.sh main pr "docs: readme")
expect_match "docs PR cuts no release"   'Merging \*\*cuts no release\*\*' "$out"
expect_match "docs PR lists none"        'Releasable files in this PR: none' "$out"
expect_match "nothing pending"           'waiting on main since `v0.1.0`: nothing' "$out"
expect_match "comment carries its marker" '^<!-- release-gate -->' "$out"
touch_file pkgs/x/a.go; commit "feat: a"
out=$(in_repo scripts/release-gate-pr.sh main pr "feat: a")
expect_match "feat PR releases minor"    'Merging \*\*releases `v0.2.0`\*\* \(minor — feat: feat: a; base v0.1.0\)' "$out"
expect_match "feat PR lists its file"    'Releasable files in this PR: `pkgs/x/a.go`' "$out"
# Something releasable already on main: a docs PR still releases, with main's bump.
git -C "$repo" checkout -q main
touch_file pkgs/y/b.go; commit "fix: b"
git -C "$repo" checkout -q -b pr2
touch_file README.md again; commit "docs: again"
out=$(in_repo scripts/release-gate-pr.sh main pr2 "docs: again")
expect_match "pending on main releases anyway" 'Merging \*\*releases `v0.1.1`\*\* \(patch — fix: fix: b; base v0.1.0\)' "$out"
expect_match "pending count"                   'waiting on main since `v0.1.0`: 1 releasable file' "$out"
fixture gatenotag
git -C "$repo" tag -d v0.1.0 >/dev/null
expect_match "no tag: unconditional" 'first merge releases unconditionally' "$(in_repo scripts/release-gate-pr.sh main main "feat: x")"

# ── merge-ready.sh ──────────────────────────────────────────────────────────────
# A pull_request run checks out refs/pull/N/merge: the PR head merged into main *as it was when
# the run started*. merge-ready.sh must call a green run stale once main has moved on. gh is
# stubbed: each call is answered from a file in the stub's directory (see stub_gh).
main_sha=1111111111111111111111111111111111111111
old_main=2222222222222222222222222222222222222222
head_sha=3333333333333333333333333333333333333333
merge_sha=4444444444444444444444444444444444444444
stub_gh() {
  stubdir="$tmp/stub-$1"; mkdir -p "$stubdir/bin"
  cat > "$stubdir/bin/gh" <<'STUB'
#!/usr/bin/env bash
d=$GH_STUB_DIR
echo "gh $*" >> "$d/calls"
case "$*" in
  "pr view "*)                           cat "$d/pr_view" ;;
  "api repos/o/r/git/ref/heads/"*)       cat "$d/main_sha" ;;
  "run list --workflow ci.yml"*)         cat "$d/run_list" ;;
  "run list --workflow merge-tree.yml"*) cat "$d/tree_runs" 2>/dev/null || true ;;
  "run view "*"--log")                   cat "$d/run_log" ;;
  "api repos/o/r/commits/"*)             cat "$d/parents" ;;
  "pr checks "*)                         cat "$d/checks" ;;
  "run rerun "*|"workflow run "*) ;;
  *) echo "gh stub: unexpected: $*" >&2; exit 99 ;;
esac
STUB
  chmod +x "$stubdir/bin/gh"
  printf '%s main\n' "$head_sha" > "$stubdir/pr_view"
  printf '%s\n' "$main_sha" > "$stubdir/main_sha"
  printf '99 completed success\n' > "$stubdir/run_list"
  printf 'affected\tfetch +%s:refs/remotes/pull/7/merge\n' "$merge_sha" > "$stubdir/run_log"
  printf '%s\n%s\n' "$main_sha" "$head_sha" > "$stubdir/parents"
  printf 'pass\npass\nskipping\n' > "$stubdir/checks"
}
# ready <name> [args...]: runs merge-ready.sh 7 against stub <name>; sets out and rc.
ready() {
  local name=$1; shift
  set +e
  out=$(GH_STUB_DIR="$tmp/stub-$name" PATH="$tmp/stub-$name/bin:$PATH" MERGE_READY_REPO=o/r "$src/scripts/merge-ready.sh" 7 "$@" 2>&1)
  rc=$?
  set -e
}
stub_gh fresh;  ready fresh
expect_eq    "fresh run exits 0"            0 "$rc"
expect_match "fresh run lists main SHA"     "main +$main_sha" "$out"
expect_match "fresh run lists merge SHA"    "merge +$merge_sha" "$out"
expect_match "fresh run lists check head"   "head +$head_sha" "$out"
expect_match "fresh run counts checks"      '3 checks' "$out"

stub_gh stale; printf '%s\n%s\n' "$old_main" "$head_sha" > "$tmp/stub-stale/parents"; ready stale
expect_eq    "stale base exits 2"           2 "$rc"
expect_match "stale base is named"          'STALE' "$out"
expect_match "stale shows the old base"     "base +$old_main" "$out"
ready stale --revalidate
expect_eq    "stale + revalidate still 2"   2 "$rc"
expect_match "revalidate dispatches merge-tree.yml" 'workflow run merge-tree.yml' "$(cat "$tmp/stub-stale/calls")"
if grep -q 'run rerun' "$tmp/stub-stale/calls"; then bad "revalidate must not use gh run rerun (reuses the old merge ref)"; else ok; fi
# A successful dispatch run for this very pr / head / main also counts as the composite check.
printf '77 completed success merge-tree pr=7 head=%s base=%s\n' "$head_sha" "$main_sha" > "$tmp/stub-stale/tree_runs"; ready stale
expect_eq    "stale + matching merge-tree run exits 0" 0 "$rc"
expect_match "merge-tree run is reported"   'merge-tree run 77' "$out"
printf '77 completed success merge-tree pr=7 head=%s base=%s\n' "$head_sha" "$old_main" > "$tmp/stub-stale/tree_runs"; ready stale
expect_eq    "merge-tree run on another base does not count" 2 "$rc"

stub_gh otherhead; printf '%s\n%s\n' "$main_sha" "9999999999999999999999999999999999999999" > "$tmp/stub-otherhead/parents"; ready otherhead
expect_eq    "run for another head exits 2" 2 "$rc"
stub_gh pending; printf 'pass\npending\n' > "$tmp/stub-pending/checks"; ready pending
expect_eq    "pending check exits 1"        1 "$rc"
stub_gh failing; printf 'pass\nfail\n' > "$tmp/stub-failing/checks"; ready failing
expect_eq    "failed check exits 1"         1 "$rc"
stub_gh cancel; printf 'pass\ncancel\n' > "$tmp/stub-cancel/checks"; ready cancel
expect_eq    "cancelled check exits 1"      1 "$rc"
stub_gh running; printf '99 in_progress \n' > "$tmp/stub-running/run_list"; ready running
expect_eq    "unfinished run exits 1"       1 "$rc"
stub_gh norun; : > "$tmp/stub-norun/run_list"; ready norun
expect_eq    "no pull_request run exits 1"  1 "$rc"
stub_gh nochecks; : > "$tmp/stub-nochecks/checks"; ready nochecks
expect_eq    "no checks at all exits 1"     1 "$rc"
set +e; "$src/scripts/merge-ready.sh" >/dev/null 2>&1; rc=$?; set -e
expect_eq    "no PR number exits 3"         3 "$rc"

# ── merge-tree-check.sh ─────────────────────────────────────────────────────────
# The #187 / #188 shape: one branch adds a parameter to a constructor and fixes the callers it
# knows, the other adds a new file calling the old signature. git merges them without a conflict
# and each branch builds on its own — only the merged tree fails, which is what the check builds.
if command -v go >/dev/null 2>&1; then
  export GOFLAGS=-mod=mod GOWORK=off GOTOOLCHAIN=local
  fixture tree
  printf 'module fx\n\ngo 1.21\n' > "$repo/go.mod"
  printf 'package fx\n\nfunc NewCreateOrder(a, b int) int { return a + b }\n' > "$repo/order.go"
  printf 'package fx\n\nimport "testing"\n\nfunc TestOrder(t *testing.T) { _ = NewCreateOrder(1, 2) }\n' > "$repo/order_test.go"
  commit "feat: base"
  git -C "$repo" update-ref refs/remotes/origin/main main
  git -C "$repo" checkout -q -b ownership
  printf 'package fx\n\nfunc NewCreateOrder(a, b, policy int) int { return a + b + policy }\n' > "$repo/order.go"
  printf 'package fx\n\nimport "testing"\n\nfunc TestOrder(t *testing.T) { _ = NewCreateOrder(1, 2, 0) }\n' > "$repo/order_test.go"
  commit "feat: ownership policy"
  git -C "$repo" checkout -q main
  git -C "$repo" checkout -q -b retry
  printf 'package fx\n\nimport "testing"\n\nfunc TestRetry(t *testing.T) { _ = NewCreateOrder(3, 4) }\n' > "$repo/retry_test.go"
  commit "test: retry"
  git -C "$repo" checkout -q main
  tree_check() { set +e; tc_out=$(in_repo scripts/merge-tree-check.sh "$@" 2>&1); tc_rc=$?; set -e; }
  tree_check main ownership; expect_eq "ownership alone builds"   0 "$tc_rc"
  tree_check main retry;     expect_eq "retry alone builds"       0 "$tc_rc"
  if git -C "$repo" merge-tree --write-tree ownership retry >/dev/null 2>&1; then ok; else bad "fixture: ownership + retry must merge without a text conflict"; fi
  tree_check ownership retry
  expect_eq    "merged tree fails to compile" 1 "$tc_rc"
  expect_match "failure names the call"       'NewCreateOrder' "$tc_out"
  git -C "$repo" checkout -q -b clash main
  echo "package fx // clash" > "$repo/order.go"; commit "feat: clash"
  git -C "$repo" checkout -q -b clash2 main
  echo "package fx // other" > "$repo/order.go"; commit "feat: clash2"
  git -C "$repo" checkout -q main
  tree_check clash clash2
  expect_eq    "text conflict exits 2"        2 "$tc_rc"
  if [ -n "$(git -C "$repo" worktree list | sed 1d)" ]; then bad "merge-tree-check.sh left a worktree behind"; else ok; fi
  unset GOFLAGS GOWORK GOTOOLCHAIN
else
  echo "scripts/test.sh: go not found — merge-tree-check.sh fixture skipped" >&2
fi

echo "scripts/test.sh: $pass passed, $fail failed"
[ "$fail" = 0 ]
