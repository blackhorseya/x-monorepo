#!/usr/bin/env bash
# Tests for the release / CI decision scripts: scripts/ci-changes.sh (which ci.yml jobs a change
# runs), scripts/releasable.sh (does a change cut a release), scripts/next-version.sh (which
# version), scripts/release-gate-pr.sh (the PR comment built from the three) and
# scripts/red-stage.sh (where a RED `go test` run failed). A wrong rule in
# ci-changes.sh skips a job silently — CI stays green — so the rules are pinned here.
#
#   scripts/test.sh          # task scripts-test; CI's scripts-test job
#
# Each case builds a throwaway git repo with scripts/ copied in and runs the copies there
# (ci-changes.sh cd's to its own repo root; release-gate-pr.sh calls scripts/ relative to cwd).
# Plain bash 3.2 (macOS) and 5 (ubuntu): no mapfile, no associative arrays.
set -euo pipefail
# Run under the outer gate (#307): a process group of its own, checked for leftovers on any exit.
[ -n "${SUITE_GUARD:-}" ] || exec "$(dirname "$0")/suite-guard.sh" "$0" "$@"
src=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
# The fixtures must not inherit the developer's signing / hook settings.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
PB_PIDS=""   # dex-prebind stubs; killed by the one EXIT trap set below the prepull helpers
trap 'test -z "$PB_PIDS" || kill $PB_PIDS 2>/dev/null; rm -rf "$tmp"' EXIT
fail=0 pass=0
# shellcheck source=fault-inject.sh
. "$src/scripts/best-effort.sh"    # best_effort: cleanup that must not decide a verdict, and says so when it fails (#462)
. "$src/scripts/fault-inject.sh"   # inject_fail: a PATH wrapper that runs the real tool, then dies (#462)

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
# count_lines <grep -c args>: the number of matching lines; 1 (no match) is 0, anything above is a failure (exit 2). Meant for an ASSIGNMENT (n=$(… | count_lines x)): inside an argument the exit never reaches set -e (#462)
count_lines() { local n rc=0; n=$(grep -c "$@") || rc=$?; if [ "$rc" -gt 1 ]; then echo "test.sh: grep failed with exit $rc in count_lines $*" >&2; exit 2; fi; echo "$n"; }

# ── ci-changes.sh ───────────────────────────────────────────────────────────────
all_jobs="gen_freshness lint work_sync coverage build breaking release_snapshot actions_resolve web console_e2e console_image helm scripts_test helm_kind helm_kind_keda k6 k6_auth k6_wire"
# jobs_on <name> <files...>: commits the files on a branch off main and returns the jobs set to true.
jobs_on() {
  local name=$1; shift
  fixture "$name"
  git -C "$repo" checkout -q -b change
  for f in "$@"; do touch_file "$f"; done
  commit "chore: $name"
  in_repo scripts/ci-changes.sh | sed -n 's/^run_\(.*\)=true$/\1/p' | tr '\n' ' ' | sed 's/ $//'
}
# Shorthands for the sets below (#328): the Go checks every Go change runs, and what only product code adds.
go_checks="gen_freshness lint work_sync coverage"
go_prod_pre="build console_e2e helm_kind"; go_prod="$go_prod_pre k6_wire"
expect_eq "docs only runs nothing"            "" "$(jobs_on docs README.md docs/a.md)"
# The Grafana dashboard and alert rules are inputs of helm-check's check-observability.py (#367 slice 5c): a metric-name typo in a rule that never fires is caught there, so a change to them runs helm and nothing else.
expect_eq "grafana provisioning runs helm-check only" "helm" "$(jobs_on grafana deployments/docker/grafana/provisioning/alerting.yaml)"
expect_eq "a grafana dashboard runs helm-check only"  "helm" "$(jobs_on grafanadash deployments/docker/grafana/dashboards/rpc.json)"

# A change to *_test.go alone runs the Go checks and the test matrix, no integration job (#328).
expect_eq "a _test.go alone runs only the Go checks" "$go_checks" "$(jobs_on gotest pkgs/ordering/domain/order/order_test.go)"
expect_eq "a platform/server _test.go is still test-only"  "$go_checks" "$(jobs_on servertest pkgs/platform/server/limits_test.go)"
expect_eq "an identity _test.go is still test-only"        "$go_checks" "$(jobs_on idtest pkgs/identity/x_test.go)"
expect_eq "an apps/worker _test.go is still test-only"     "$go_checks" "$(jobs_on workertest apps/worker/x_test.go)"
expect_eq "an apps/rpc _test.go is still test-only"        "$go_checks" "$(jobs_on rpctest apps/rpc/x_test.go)"
# A test helper that is not named *_test.go is product code: it can be imported by a binary.
expect_eq "a non-_test.go test helper is product code" \
  "$go_checks build console_e2e helm_kind helm_kind_keda k6_wire" \
  "$(jobs_on pgtest pkgs/platform/postgres/pgtest/pgtest.go)"
expect_eq "a _test.go next to a product file changes nothing about the product file" \
  "$go_checks $go_prod" \
  "$(jobs_on gomixed pkgs/ordering/domain/order/order.go pkgs/ordering/domain/order/order_test.go)"

# Product Go outside the auth / k6 / worker paths: no release-snapshot, console-image, keda, k6, k6-auth.
expect_eq "go runs the Go checks, kind and k6-wire" \
  "$go_checks $go_prod" \
  "$(jobs_on go pkgs/ordering/domain/order/order.go)"
# go.mod / go.sum / go.work change the dependency graph of every binary: release-snapshot, keda and k6 run too.
gomod_jobs="$go_checks build release_snapshot console_e2e helm_kind helm_kind_keda k6 k6_wire"
expect_eq "go.mod counts as go, and as a release / keda / k6 input" "$gomod_jobs" "$(jobs_on gomod pkgs/ordering/go.mod)"
expect_eq "go.sum likewise"  "$gomod_jobs" "$(jobs_on gosum pkgs/ordering/go.sum)"
expect_eq "go.work likewise" "$gomod_jobs" "$(jobs_on gowork go.work)"
expect_eq "go.work.sum likewise" "$gomod_jobs" "$(jobs_on goworksum go.work.sum)"
expect_eq ".golangci.yml is product code (not test-only)" "$go_checks $go_prod" "$(jobs_on golangci .golangci.yml)"
# proto: generated Go is committed (a Go change) and the TS client is generated from it (a web change),
# so console-image keeps the trigger; it is not a release-snapshot input.
expect_eq "proto is go + breaking + web, console-image included" \
  "$go_checks build breaking web console_e2e console_image helm_kind k6_wire" \
  "$(jobs_on proto protos/ordering/v1/service.proto)"
expect_eq "web only: no lint, no k6, no keda" \
  "gen_freshness web console_e2e console_image helm_kind" \
  "$(jobs_on web web/apps/console/src/routes/orders.tsx)"
expect_eq "helm only" "helm helm_kind helm_kind_keda" "$(jobs_on helm deployments/helm/x-monorepo/values.yaml)"
# scripts/test.sh drives deployments/helm/kind/coredns-dex.sh itself (#287): a change to only that
# file must run scripts-test, or its fixtures cannot catch a regression in it at PR time.
expect_eq "coredns-dex.sh also runs scripts-test" "helm scripts_test helm_kind helm_kind_keda" "$(jobs_on coredns deployments/helm/kind/coredns-dex.sh)"
expect_eq "any kind script runs scripts-test (check-exit-swallow.sh scans every tracked *.sh, #462)" "helm scripts_test helm_kind helm_kind_keda" "$(jobs_on kindsmoke deployments/helm/kind/smoke.sh)"
expect_eq "a k6 shell script runs scripts-test too" "scripts_test k6 k6_auth k6_wire" "$(jobs_on k6sh tests/k6/catalog.sh)"
expect_eq "a console e2e shell script runs scripts-test too" "gen_freshness web console_e2e console_image scripts_test helm_kind" "$(jobs_on e2esh web/apps/console/e2e/image.sh)"
# test.sh also reads .github/workflows/k6-bench.yml (wiring assertions) and copies .goreleaser.yaml
# into the cosign-smoke fixture (#298): each alone must run scripts-test; no other workflow does.
expect_eq "k6-bench.yml also runs scripts-test" "actions_resolve scripts_test" "$(jobs_on k6bench .github/workflows/k6-bench.yml)"
# test.sh reads docs/ci.md too (#328: every job named there, every ci.yml flag wired): a docs-only
# change to it must run scripts-test, and no other doc does.
expect_eq "docs/ci.md also runs scripts-test" "scripts_test" "$(jobs_on cidoc docs/ci.md)"
expect_eq "another doc runs nothing"          ""             "$(jobs_on otherdoc docs/operations.md)"
# The oidc-dex Playwright project runs against the compose Dex (#274): its config or the compose file alone
# must run console-e2e, or a Dex change (CORS origins, redirect URIs) is only found after merge.
expect_eq "dex config runs console-e2e" "console_e2e" "$(jobs_on dexcfg deployments/docker/dex/config.yaml)"
expect_eq "compose file runs console-e2e" "console_e2e" "$(jobs_on composefile deployments/docker/docker-compose.yaml)"

# A rename is a delete plus an add. `git diff --name-only` pairs them and prints only the new path, so
# helper.go -> helper_test.go would look test-only while it removed a product file: ci-changes.sh
# diffs with --no-renames and has_prod sees both paths. rename_jobs_on <name> <old> <new>.
rename_jobs_on() {
  local name=$1 old=$2 new=$3
  fixture "$name"
  touch_file "$old" "package x"; commit "chore: $old"
  git -C "$repo" update-ref refs/remotes/origin/main main
  git -C "$repo" checkout -q -b change
  mkdir -p "$repo/$(dirname "$new")"
  git -C "$repo" mv "$old" "$new"
  commit "chore: rename"
  in_repo scripts/ci-changes.sh | sed -n 's/^run_\(.*\)=true$/\1/p' | tr '\n' ' ' | sed 's/ $//'
}
expect_eq "renaming a product file to _test.go removed a product input" \
  "$go_checks $go_prod" "$(rename_jobs_on torename pkgs/ordering/helper.go pkgs/ordering/helper_test.go)"
expect_eq "renaming a _test.go to a product file added a product input" \
  "$go_checks $go_prod" "$(rename_jobs_on fromtest pkgs/ordering/helper_test.go pkgs/ordering/helper.go)"
expect_eq "renaming a _test.go to another _test.go stays test-only" \
  "$go_checks" "$(rename_jobs_on testtotest pkgs/ordering/a_test.go pkgs/ordering/b_test.go)"
expect_eq "moving a product file into pkgs/platform/server counts both ends (k6, k6-auth, keda)" \
  "$go_checks $go_prod_pre helm_kind_keda k6 k6_auth k6_wire" \
  "$(rename_jobs_on movesrv pkgs/ordering/limits.go pkgs/platform/server/limits.go)"

# #448: the auth matrix script is the k6-auth input; moving it out of tests/k6 (or a file into it) keeps both ends.
expect_eq "moving the auth matrix out of tests/k6 still runs k6, k6-auth (--no-renames)" \
  "scripts_test k6 k6_auth k6_wire" "$(rename_jobs_on movematrix tests/k6/auth-matrix.sh docs/auth-matrix.sh)"
expect_eq "moving a script into tests/k6 runs k6, k6-auth (--no-renames)" \
  "scripts_test k6 k6_auth k6_wire" "$(rename_jobs_on intok6 docs/auth-matrix.sh tests/k6/auth-matrix.sh)"
expect_eq "moving a file out of apps/worker still runs keda and k6" \
  "$go_checks $go_prod_pre helm_kind_keda k6 k6_wire" \
  "$(rename_jobs_on moveworker apps/worker/main.go pkgs/ordering/main.go)"
expect_eq "moving a file out of the chart still runs helm and the kind jobs" \
  "helm helm_kind helm_kind_keda" \
  "$(rename_jobs_on movehelm deployments/helm/x-monorepo/values.yaml docs/values.yaml)"

# k6 (the smoke: rate limit, timeout, load): tests/k6, pkgs/platform/server (interceptors, limits.go),
# pkgs/platform/config (their env and defaults), apps/rpc, apps/worker, configs, go.mod. A business-flow
# change (pkgs/ordering…) is covered by helm-kind, not by k6 (#328). k6-auth is the auth subset, k6-wire follows product Go.
expect_eq "k6 scripts: all three k6 jobs"  "k6 k6_auth k6_wire" "$(jobs_on k6 tests/k6/orders.js)"
expect_eq "platform/server: k6, k6-auth, keda, k6-wire" \
  "$go_checks $go_prod_pre helm_kind_keda k6 k6_auth k6_wire" "$(jobs_on server pkgs/platform/server/limits.go)"
expect_eq "platform/config carries the limits' env: k6, k6-auth, keda" \
  "$go_checks $go_prod_pre helm_kind_keda k6 k6_auth k6_wire" "$(jobs_on config pkgs/platform/config/config.go)"
expect_eq "platform elsewhere: keda, no k6" \
  "$go_checks $go_prod_pre helm_kind_keda k6_wire" "$(jobs_on platform pkgs/platform/usecasex/usecasex.go)"
expect_eq "identity: k6-auth only"      "$go_checks $go_prod_pre k6_auth k6_wire" "$(jobs_on identity pkgs/identity/provider.go)"
expect_eq "apps/rpc: k6, k6-auth"       "$go_checks $go_prod_pre k6 k6_auth k6_wire" "$(jobs_on rpc apps/rpc/main.go)"
expect_eq "apps/worker: k6, keda"       "$go_checks $go_prod_pre helm_kind_keda k6 k6_wire" "$(jobs_on worker apps/worker/main.go)"
expect_eq "configs: k6 and k6-auth, not keda" \
  "$go_checks $go_prod_pre k6 k6_auth k6_wire" "$(jobs_on configs configs/config.example.yaml)"
expect_eq "apps/catalog-rpc: k6, k6-auth (rpc asks it for every order)" \
  "$go_checks $go_prod_pre k6 k6_auth k6_wire" "$(jobs_on catalogrpc apps/catalog-rpc/main.go)"
expect_eq "pkgs/catalog (the client adapter, the forwarder): k6, k6-auth" \
  "$go_checks $go_prod_pre k6 k6_auth k6_wire" "$(jobs_on catalogpkg pkgs/catalog/adapters/client/client.go)"
expect_eq "a catalog test alone is no k6 input" "$go_checks" "$(jobs_on catalogtest pkgs/catalog/adapters/client/client_test.go)"
expect_eq "ordering is not a k6 input"  "$go_checks $go_prod" "$(jobs_on ordering pkgs/ordering/adapters/rpc/handler.go)"
expect_eq "inventory is not a k6 input" "$go_checks $go_prod" "$(jobs_on inventory pkgs/inventory/usecase/x.go)"

# release-snapshot: goreleaser, a Dockerfile, .dockerignore, release.yml, go.mod / go.sum / go.work. Not the console image
# (.goreleaser.yaml: the console has no Go binary, release.yml pushes it with buildx).
expect_eq "Dockerfile is an image change" \
  "release_snapshot console_image helm_kind helm_kind_keda" \
  "$(jobs_on image deployments/docker/Dockerfile.rpc)"
expect_eq ".dockerignore is an image change" \
  "release_snapshot console_image helm_kind helm_kind_keda" \
  "$(jobs_on dockerignore .dockerignore)"
expect_eq "the console Dockerfile is not a release-snapshot input" \
  "gen_freshness web console_e2e console_image helm_kind helm_kind_keda" \
  "$(jobs_on consoleimg web/apps/console/Dockerfile)"
expect_eq "the console nginx config is not a release-snapshot input" \
  "gen_freshness web console_e2e console_image helm_kind helm_kind_keda" \
  "$(jobs_on consolenginx web/apps/console/deploy/nginx.conf)"
expect_eq ".goreleaser.yaml is an image change, and a scripts-test input (cosign-smoke fixture)" \
  "release_snapshot console_image scripts_test helm_kind helm_kind_keda" \
  "$(jobs_on goreleaser .goreleaser.yaml)"
expect_eq "another workflow is actions-resolve only" "actions_resolve" "$(jobs_on wf .github/workflows/labeler.yml)"
# release.yml pins cosign-release (#215): a bump there must run release-snapshot's cosign smoke and
# Rekor canary, which ci-changes.sh used to skip (release.yml only set the actions area).
expect_eq "release.yml runs release-snapshot too, and scripts-test (its image loop is a list case input)" "release_snapshot actions_resolve scripts_test" "$(jobs_on relyml .github/workflows/release.yml)"
expect_eq "release-drafter.yml runs actions-resolve and scripts-test (its Images block is a list case input)" "actions_resolve scripts_test" "$(jobs_on drafter .github/release-drafter.yml)"
expect_eq "ci.yml runs everything"   "$all_jobs" "$(jobs_on ciyml .github/workflows/ci.yml)"
expect_eq "Taskfile runs everything" "$all_jobs" "$(jobs_on taskfile Taskfile.yml)"
expect_eq "scripts/ runs everything" "$all_jobs" "$(jobs_on scripts scripts/new.sh)"

fixture ciall
expect_eq "CI_ALL=1 runs everything" "$all_jobs" "$(in_repo env CI_ALL=1 scripts/ci-changes.sh | sed -n 's/^run_\(.*\)=true$/\1/p' | tr '\n' ' ' | sed 's/ $//')"
expect_eq "CI_ALL prints one flag per job" "$(echo "$all_jobs" | wc -w | tr -d ' ')" "$(in_repo env CI_ALL=1 scripts/ci-changes.sh | wc -l | tr -d ' ')"
# assignments, not `"$(… || [ $? -eq 1 ])"` inside an argument: the status of a substitution that is an argument never reaches set -e (#462)
all_out=$(in_repo env CI_ALL=1 scripts/ci-changes.sh)
n_false=$(grep -c '=false$' <<<"$all_out" || [ $? -eq 1 ])
expect_eq "CI_ALL=1 prints no false" "0" "$n_false"
# Every flag is a job's `if:` in ci.yml, declared once as an output of `affected` (a flag nobody reads skips nothing).
for j in $all_jobs; do
  _c220a=$(count_lines "^      run_$j: \${{ steps.jobs.outputs.run_$j }}\$" "$src/.github/workflows/ci.yml"); expect_eq "ci.yml declares run_$j as an output of affected" 1 "$_c220a"
  _c221a=$(count_lines "^    if: needs.affected.outputs.run_$j == 'true'\$" "$src/.github/workflows/ci.yml"); expect_eq "ci.yml has exactly one job gated on run_$j"       1 "$_c221a"
  expect_match "docs/ci.md names the $j job" "(^|[^a-z0-9-])$(echo "$j" | tr _ -)([^a-z0-9-]|\$)" "$(cat "$src/docs/ci.md")"
done
# ci_job <name>: the lines of that top-level job in ci.yml.
ci_job() { awk -v n="  $1:" '$0 == n {f=1; next} /^  [a-z0-9-]+:/ || /^  #/ {f=0} f' "$src/.github/workflows/ci.yml"; }
# helm-kind, helm-kind-keda and k6-auth / k6-wire are jobs of their own, not matrix legs: a skipped one
# leaves a skipped check, a dynamic matrix would leave none (#328).
_c228a=$(count_lines 'task: \[helm-kind' "$src/.github/workflows/ci.yml"); expect_eq "no matrix over the kind tasks" 0 "$_c228a"
_c229a=$(ci_job helm-kind | count_lines '^      - run: task helm-kind$'); expect_eq "helm-kind job runs task helm-kind"            1 "$_c229a"
_c230a=$(ci_job helm-kind-keda | count_lines '^      - run: task helm-kind-keda$'); expect_eq "helm-kind-keda job runs task helm-kind-keda"  1 "$_c230a"
_c231a=$(ci_job helm-kind | count_lines 'matrix'); expect_eq "helm-kind has no matrix"      0 "$_c231a"
_c232a=$(ci_job helm-kind-keda | count_lines 'matrix'); expect_eq "helm-kind-keda has no matrix" 0 "$_c232a"
_c233a=$(ci_job k6-auth | count_lines '^      - run: task k6-auth-matrix$'); expect_eq "k6-auth job runs task k6-auth-matrix"        1 "$_c233a"
_c234a=$(ci_job k6-auth | count_lines 'task k6-wire'); expect_eq "k6-auth job no longer runs task k6-wire"     0 "$_c234a"
# #448: k6-auth is one job with a static matrix of shards, K6_AUTH_SHARD=<i>/<n>. The shards must be exactly
# 1/n … n/n, and their cells together must be the whole list, each once — a cell in no shard would never run.
m=$src/tests/k6/auth-matrix.sh
all_cells=$(K6_AUTH_LIST=1 "$m")
expect_eq "auth matrix lists 27 cells" 27 "$(printf '%s\n' "$all_cells" | wc -l | tr -d ' ')"
expect_eq "auth matrix cell names are unique" 0 "$(printf '%s\n' "$all_cells" | sort | uniq -d | wc -l | tr -d ' ')"
shards=$(ci_job k6-auth | sed -n 's/^        shard: \[\(.*\)\]$/\1/p' | tr -d '" ' | tr ',' ' ')
expect_match "k6-auth has a static shard matrix" '^1/[1-9]( |$)' "$shards"
n=${shards##*/}
want=""; for ((i = 1; i <= n; i++)); do want="$want $i/$n"; done
expect_eq "k6-auth shards are 1/n … n/n" "${want# }" "$shards"
_c246a=$(ci_job k6-auth | count_lines '^      K6_AUTH_SHARD: ${{ matrix.shard }}$'); expect_eq "k6-auth leg reads its shard from the matrix" 1 "$_c246a"
_c247a=$(ci_job k6-auth | count_lines '^      fail-fast: false$'); expect_eq "k6-auth matrix does not stop the other legs" 1 "$_c247a"
union=""
for ((i = 1; i <= n; i++)); do
  part=$(K6_AUTH_LIST=1 K6_AUTH_SHARD=$i/$n "$m")
  expect_match "auth matrix shard $i/$n is not empty" '.' "$part"
  union="$union
$part"
done
expect_eq "the union of the k6-auth shards is the whole cell list, each cell once" \
  "$(printf '%s\n' "$all_cells" | sort)" "$(printf '%s\n' "$union" | sed '/^$/d' | sort)"
# A name that is not a cell is an error (it used to run nothing and pass), and so is a shard that is not i/n.
rc=0; K6_AUTH_LIST=1 "$m" no-such-cell >/dev/null 2>&1 || rc=$?
expect_eq "an unknown cell name fails" 1 "$rc"
rc=0; K6_AUTH_LIST=1 "$m" whoami-503 >/dev/null 2>&1 || rc=$?
expect_eq "a known cell name passes" 0 "$rc"
rc=0; K6_AUTH_LIST=1 K6_AUTH_SHARD=$((n + 1))/$n "$m" >/dev/null 2>&1 || rc=$?
expect_eq "a shard beyond n fails" 2 "$rc"
rc=0; K6_AUTH_LIST=1 K6_AUTH_SHARD=1/$n "$m" whoami-503 >/dev/null 2>&1 || rc=$?
expect_eq "a shard together with named cells fails" 2 "$rc"
# #209: the key must stay out of k6's output and tags — k6-wire is the job that proves it, on its own now.
_c267a=$(ci_job k6-wire | count_lines '^      - run: task k6-wire$'); expect_eq "k6-wire job runs task k6-wire"               1 "$_c267a"
expect_eq "k6-wire needs no Postgres (docs/ci.md says so)" 0 "$(ci_job k6-wire | grep -ci 'postgres')"

# ── setup / playwright actions: who may write a cache, and what a job reads (#450) ──────────────────────────────────
# A pull request whose cache key missed used to save a full set of entries on its own ref: two PRs doing that pushed repo
# cache usage to 14.8 GB (limit 10 GB), main's entries were evicted and every later job compiled cold. plan.sh is the one
# place that says who may write; each cache step below reads its output, and each read-write step has a restore-only twin.
plan="$src/.github/actions/setup/plan.sh"
# count_lines <grep args>: how many lines of stdin match. grep exits 1 for "none" (that is a 0); any other non-zero exit is a failure of
# the check itself and stops the suite (a `|| true` here would turn a broken grep into a quiet 0).
# plan_out <event> <ref> <default branch> <scope> <output>: one output of plan.sh; PLAN-FAILED / NO-KEY when the script failed or lacks the key,
# so that an expectation of "" cannot pass on a crashed script.
plan_out() {
  local o rc=0
  o=$(EVENT_NAME=$1 REF=$2 DEFAULT_BRANCH=$3 SCOPE=$4 GITHUB_OUTPUT=/dev/stdout "$plan") || rc=$?
  if [ "$rc" != 0 ]; then echo PLAN-FAILED; return; fi
  n_key=$(printf '%s\n' "$o" | count_lines "^$5=")
  if [ "$n_key" != 1 ]; then echo NO-KEY; return; fi
  printf '%s\n' "$o" | sed -n "s/^$5=//p"
}
# GitHub (docs "Cache access for low-trust workflow triggers"): only push / workflow_dispatch / repository_dispatch / delete /
# registry_package / page_build / schedule may create caches in the default branch's scope; workflow_run, pull_request_target,
# issue_comment ... are read-only there. This repo writes from push and workflow_dispatch only.
expect_eq "plan: push on main writes"                         true  "$(plan_out push refs/heads/main main lint write)"
expect_eq "plan: workflow_dispatch on main writes"            true  "$(plan_out workflow_dispatch refs/heads/main main lint write)"
expect_eq "plan: a pull request never writes"                 false "$(plan_out pull_request refs/pull/7/merge main lint write)"
expect_eq "plan: pull_request_target on main never writes"    false "$(plan_out pull_request_target refs/heads/main main lint write)"
expect_eq "plan: workflow_run on main never writes"           false "$(plan_out workflow_run refs/heads/main main lint write)"
expect_eq "plan: schedule on main does not write"             false "$(plan_out schedule refs/heads/main main lint write)"
expect_eq "plan: issue_comment on main never writes"          false "$(plan_out issue_comment refs/heads/main main lint write)"
expect_eq "plan: push on another branch does not write"       false "$(plan_out push refs/heads/feat main lint write)"
expect_eq "plan: dispatch on another branch does not write"   false "$(plan_out workflow_dispatch refs/heads/feat main lint write)"
expect_eq "plan: a tag push does not write"                   false "$(plan_out push refs/tags/v1.0.0 main lint write)"
expect_eq "plan: a branch that only starts with main does not write" false "$(plan_out push refs/heads/mainx main lint write)"
expect_eq "plan: no default branch known does not write"      false "$(plan_out push refs/heads/main '' lint write)"
# the build cache is written by the designated saver only, and only when the run may write
expect_eq "plan: the saver of a group writes its build cache" true  "$(plan_out push refs/heads/main main build save_build)"
expect_eq "plan: a non-saver of the group does not"           false "$(plan_out push refs/heads/main main k6 save_build)"
expect_eq "plan: the saver does not write on a pull request"  false "$(plan_out pull_request refs/pull/7/merge main build save_build)"
expect_eq "plan: an unlisted scope gets no build cache"       ""    "$(plan_out push refs/heads/main main some-new-job group)"
expect_eq "plan: an unlisted scope never saves a build cache" false "$(plan_out push refs/heads/main main some-new-job save_build)"
expect_match "plan: the week is an ISO week"                  '^[0-9]{4}-W[0-9]{2}$' "$(plan_out push refs/heads/main main lint week)"
# Every job of ci.yml that calls the setup action (and keeps the Go cache) maps to a build-cache group, with exactly one saver per
# group; a new job that is not in plan.sh fails here instead of silently getting no build cache.
ci_setup_scopes() {
  local j m jobs mods
  jobs=$(awk '/^  [a-z0-9-]+:$/ {gsub(/[ :]/, ""); job=$0} /uses: \.\/\.github\/actions\/setup/ {print job}' "$src/.github/workflows/ci.yml" | sort -u)   # assigned first: a failing awk is an error, not an empty list (#462)
  for j in $jobs; do
    n_nocache=$(ci_job "$j" | count_lines 'go-cache: "false"')
    if [ "$j" = test ]; then
      mods=$(awk '/^use \(/ {f=1; next} /^\)/ {f=0} f {gsub(/[ \t]|^\.\//, ""); print}' "$src/go.work" | sed 's#^\./##')
      for m in $mods; do echo "test-$m"; done
    elif [ "$n_nocache" = 0 ]; then
      echo "$j"
    fi
  done
}
scopes=$(ci_setup_scopes)
expect_match "ci.yml scopes: the test matrix is covered" "test-apps/rpc" "$scopes"
expect_match "ci.yml scopes: release-snapshot is covered" "release-snapshot" "$scopes"
unmapped="" groups=""
for sc in $scopes release-snapshot; do
  g=$(plan_out push refs/heads/main main "$sc" group)
  [ -n "$g" ] || unmapped="$unmapped $sc"
  groups="$groups $g"
done
expect_eq "plan: every scope of a job that calls the setup action has a build-cache group" "" "$unmapped"
ugroups=$(printf '%s\n' $groups | sort -u)
for g in $ugroups; do
  n=0; for sc in $scopes; do
    [ "$(plan_out push refs/heads/main main "$sc" group)" = "$g" ] && [ "$(plan_out push refs/heads/main main "$sc" save_build)" = true ] && n=$((n + 1))
  done
  expect_eq "plan: group $g has exactly one saver among ci.yml's jobs" 1 "$n"
done
# Matrix jobs: a leg that is not named by its own cache-scope shares the job's scope with its siblings (#452's k6-auth legs), so a saver
# among them would race for one key ("Unable to reserve", and the legs that lost kept missing). Every explicit cache-scope in the workflows
# must be a form this test understands, and no designated saver may be a matrix job unless the scope names the leg (the test matrix:
# test-<module>, saver test-apps/rpc).
unknown_forms=""
for f in "$src/.github/workflows/ci.yml" "$src/.github/workflows/release.yml"; do
  [ -f "$f" ] || continue
  while IFS= read -r line; do
    v=${line#*cache-scope:}; v=${v//[\" ]/}; v=${v%\}}
    case $v in
      test-\$\{\{matrix.module\}\}) ;;                        # the test matrix: one scope per module, mapped above
      release-snapshot) [ -n "$(plan_out push refs/heads/main main release-snapshot group)" ] || unknown_forms="$unknown_forms $v" ;;
      *) unknown_forms="$unknown_forms [$v]" ;;
    esac
  done <<EOF_SCOPES
$(awk '/cache-scope:/' "$f")
EOF_SCOPES
done
expect_eq "ci.yml / release.yml: every explicit cache-scope is a form plan.sh knows (a matrix leg needs its own, mapped scope)" "" "$unknown_forms"
saver_matrix=""
for sc in $scopes release-snapshot; do
  if [ "$(plan_out push refs/heads/main main "$sc" save_build)" = true ] && [ "$sc" != test-apps/rpc ]; then
    n_matrix=$(ci_job "$sc" | count_lines 'matrix:')
    [ "$n_matrix" = 0 ] || saver_matrix="$saver_matrix $sc"
  fi
done
expect_eq "plan: a group's saver (other than the test matrix's apps/rpc leg) is not a matrix job" "" "$saver_matrix"
race_savers() {
  local sc
  for sc in $scopes; do
    case $sc in
      test-*) if [ "$(plan_out push refs/heads/main main "$sc" save_build)" = true ]; then echo "$sc"; fi ;;
    esac
  done
}
expect_eq "plan: the race saver is exactly the apps/rpc leg of the test matrix" "test-apps/rpc" "$(race_savers)"
_c375a=$(printf '%s\n' $groups | sort -u | count_lines .); expect_eq "plan: five build-cache groups" 5 "$_c375a"
_c376a=$(printf '%s\n' $scopes | count_lines -x web); expect_eq "plan: web (go-cache false) is not given a group in ci.yml" 0 "$_c376a"

# cache_ids <action file>: ids of the steps that use actions/cache (the read-write one or /restore).
cache_ids() { awk '/^    - / {if (cache && id != "") print id; cache=0; id=""} /^    - id: / {id=$3} /^      id: / {id=$2} /uses: actions\/cache(\/restore)?@/ {cache=1} END {if (cache && id != "") print id}' "$1"; }
# step_text <file> <id>: the lines of that step.
step_text() { awk -v n="$2" '/^    - / {f=0} $0 == "    - id: " n {f=1} f' "$1"; }
step_if()   { step_text "$1" "$2" | sed -n 's/^      if: //p'; }
# step_body <file> <id>: everything that has to be identical between a read-write step and its restore-only twin.
step_body() { step_text "$1" "$2" | awk '!/^[ ]*(- id|id|if|uses):/ && !/^[ ]*#/ && !/^[ ]*$/'; }
for af in setup playwright pnpm-cache; do
  a="$src/.github/actions/$af/action.yml"
  n_cache=$(count_lines -E 'uses: actions/cache(/restore)?@' "$a")
  n_ids=$(cache_ids "$a" | count_lines .)
  expect_eq "$af: every actions/cache step has an id" "$n_cache" "$n_ids"
  _c388a=$(count_lines -E '^\s+cache: (true|pnpm|npm|yarn)' "$a"); expect_eq "$af: no other way to cache (setup-go / setup-node cache: true)" 0 "$_c388a"
  ids=$(cache_ids "$a")
  for id in $ids; do
    uses=$(step_text "$a" "$id" | sed -n 's/^      uses: \(actions\/cache[^@]*\)@.*/\1/p')
    cond=$(step_if "$a" "$id")
    case $uses in
      actions/cache)
        expect_match "$af/$id: a step that can save runs only where plan says write" "steps\.plan\.outputs\.(write|save_build) == 'true'" "$cond"
        _c396a=$(printf '%s' "$cond" | count_lines -E "plan\.outputs\.(write|save_build) !="); expect_eq "$af/$id: ... and never on a != test" 0 "$_c396a"
        twin="${id%-rw}-ro"; [ "$twin" != "${id}-ro" ] || twin=""
        expect_match "$af/$id: a read-write step is named <name>-rw" '-rw$' "$id"
        if [ -n "$twin" ]; then
          _c400a=$(cache_ids "$a" | count_lines -x "$twin"); expect_eq "$af/$id: the restore-only twin $twin exists" 1 "$_c400a"
          want=$(printf '%s' "$cond" | sed -E "s/(steps\.plan\.outputs\.(write|save_build)) == 'true'/\1 != 'true'/")
          expect_eq "$af/$id: the twin's if: is the exact opposite" "$want" "$(step_if "$a" "$twin")"
          expect_eq "$af/$id: the twin restores the same path / key / restore-keys" "$(step_body "$a" "$id")" "$(step_body "$a" "$twin")"
        fi ;;
      actions/cache/restore)
        expect_match "$af/$id: a restore-only step runs where plan does not say write" "steps\.plan\.outputs\.(write|save_build) != 'true'" "$cond" ;;
      *) bad "$af/$id: unexpected cache action [$uses]" ;;
    esac
  done
done
# The structural checks above read the text of the conditions; this evaluates them. The `if:` of every read-write / restore-only pair is
# run through a small evaluator (conjunctions of `<name> ==|!= '<literal>'` over plan.sh's outputs and the action's inputs, nothing else)
# for every event x ref, and for the build cache every scope: the pair must run exactly the step plan.sh says (rw only on a default-branch
# push / dispatch, and for the build cache only in the designated saver; the other step everywhere else), whatever way an equivalent
# expression is spelled. A condition outside that grammar is not accepted, it fails here.
# cond_runs <if: text> <write> <save_build> <group> <go-cache> <tools>: prints 1, 0, or ? when the text is outside the grammar.
cond_runs() {
  local cond=$1 write=$2 save=$3 group=$4 gocache=$5 tools=$6 term terms val re res=1
  [ -n "$cond" ] || { echo '?'; return; }
  terms=$(printf '%s\n' "$cond" | awk '{n = split($0, a, / && /); for (i = 1; i <= n; i++) print a[i]}') || { echo '?'; return; }
  re="^([A-Za-z0-9_.-]+) (==|!=) '([^']*)'\$"
  while IFS= read -r term; do
    [[ $term =~ $re ]] || { echo '?'; return; }
    case ${BASH_REMATCH[1]} in
      steps.plan.outputs.write)      val=$write ;;
      steps.plan.outputs.save_build) val=$save ;;
      steps.plan.outputs.group)      val=$group ;;
      inputs.go-cache)               val=$gocache ;;
      inputs.tools)                  val=$tools ;;
      *) echo '?'; return ;;
    esac
    if [ "${BASH_REMATCH[2]}" = "==" ]; then [ "$val" = "${BASH_REMATCH[3]}" ] || res=0; else [ "$val" != "${BASH_REMATCH[3]}" ] || res=0; fi
  done <<EOF_TERMS
$terms
EOF_TERMS
  echo "$res"
}
cells=""
for ev in push workflow_dispatch pull_request pull_request_target workflow_run schedule issue_comment; do
  for rf in refs/heads/main refs/heads/feat refs/pull/7/merge refs/tags/v1 refs/heads/mainx; do cells="$cells $ev@$rf"; done
done
# pair_check <action> <rw id> <ro id> <kind: always|build|tools> <scopes...>: "<cell> <scope> got=<rw ro> want=<rw ro>" for every disagreement
pair_check() {
  local a=$1 rw=$2 ro=$3 kind=$4 sc cell ev rf w sv g rwv rov want bad_cells=""
  shift 4
  local rwif roif
  rwif=$(step_if "$a" "$rw"); roif=$(step_if "$a" "$ro")
  for sc in "$@"; do
    for cell in $cells; do
      ev=${cell%%@*}; rf=${cell#*@}
      w=$(plan_out "$ev" "$rf" main "$sc" write); sv=$(plan_out "$ev" "$rf" main "$sc" save_build); g=$(plan_out "$ev" "$rf" main "$sc" group)
      rwv=$(cond_runs "$rwif" "$w" "$sv" "$g" true true); rov=$(cond_runs "$roif" "$w" "$sv" "$g" true true)
      if [ "$kind" = build ]; then
        if [ -z "$g" ]; then want="0 0"; elif [ "$sv" = true ]; then want="1 0"; else want="0 1"; fi
      else
        if [ "$w" = true ]; then want="1 0"; else want="0 1"; fi
      fi
      [ "$rwv $rov" = "$want" ] || bad_cells="$bad_cells [$cell $sc got=$rwv,$rov want=${want// /,}]"
    done
  done
  echo "$bad_cells"
}
sa="$src/.github/actions/setup/action.yml"; pa="$src/.github/actions/playwright/action.yml"
all_scopes="lint build k6 helm-kind test-apps/rpc test-pkgs/kernel release-snapshot some-new-job"
expect_eq "event x ref: the module cache pair runs the writer only for a default-branch push / dispatch" "" "$(pair_check "$sa" gomod-rw gomod-ro always lint build)"
expect_eq "event x ref: the build cache pair runs the writer only in the group's saver on such a run" "" "$(pair_check "$sa" gobuild-rw gobuild-ro build $all_scopes)"
expect_eq "event x ref: the tools cache pair" "" "$(pair_check "$sa" tools-rw tools-ro always lint)"
expect_eq "event x ref: the Playwright cache pair" "" "$(pair_check "$pa" cache-rw cache-ro always console-e2e)"
expect_eq "event x ref: the pnpm store cache pair" "" "$(pair_check "$src/.github/actions/pnpm-cache/action.yml" cache-rw cache-ro always web)"
# go-cache=false (web) runs neither Go cache step; tools=false runs neither tools step
none_go=""; none_tools=""
for st in gomod-rw gomod-ro gobuild-rw gobuild-ro; do
  [ "$(cond_runs "$(step_if "$sa" $st)" true true race false true)" = 0 ] || none_go="$none_go $st"
done
for st in tools-rw tools-ro; do [ "$(cond_runs "$(step_if "$sa" $st)" true true race true false)" = 0 ] || none_tools="$none_tools $st"; done
expect_eq "go-cache=false runs no Go cache step (web)" "" "$none_go"
expect_eq "tools=false runs no tools cache step" "" "$none_tools"
expect_eq "setup: the Go module cache, Go build cache and tools cache are the cache steps" "gobuild-ro gobuild-rw gomod-ro gomod-rw tools-ro tools-rw" "$(cache_ids "$src/.github/actions/setup/action.yml" | sort | tr '\n' ' ' | sed 's/ $//')"
expect_eq "playwright: cache-rw and cache-ro are the cache steps" "cache-ro cache-rw" "$(cache_ids "$src/.github/actions/playwright/action.yml" | sort | tr '\n' ' ' | sed 's/ $//')"
# key shape: the module cache is keyed on go.sum only; the build cache on group and week (never on a scope, which is what made 28 copies)
expect_match "setup: the module cache key is go.sum only"    "key: gomod-\\$\\{\\{ runner.os \\}\\}-\\$\\{\\{ hashFiles\\('\\*\\*/go.sum'\\) \\}\\}\$" "$(step_text "$src/.github/actions/setup/action.yml" gomod-rw)"
expect_match "setup: the build cache key is group and week"  "key: gobuild-.*steps\.plan\.outputs\.group.*steps\.plan\.outputs\.week" "$(step_text "$src/.github/actions/setup/action.yml" gobuild-rw)"
n_scope=$(awk '/^ +(key|restore-keys):/ || /^ +(gomod|gobuild)-/' "$src/.github/actions/setup/action.yml" | count_lines 'cache-scope')
expect_eq "setup: no cache key uses cache-scope" 0 "$n_scope"
n_loose=$(step_text "$src/.github/actions/setup/action.yml" gobuild-rw | sed -n '/restore-keys:/,$p' | count_lines 'gobuild-\${{ runner.os }}-\s*$')
expect_eq "setup: the build cache restore-keys stay inside the group" 0 "$n_loose"
# the web job needs go on PATH (task evaluates `go work edit` on every run) but compiles nothing; lint's own golangci cache must not save on a PR
_c486a=$(count_lines 'go-cache: "false"' "$src/.github/workflows/ci.yml"); expect_eq "ci.yml: only the web job opts out of the Go cache"   1 "$_c486a"
_c487a=$(ci_job web | count_lines 'go-cache: "false"'); expect_eq "ci.yml: the web job opts out of the Go cache"        1 "$_c487a"
_c488a=$(ci_job web | count_lines 'uses: ./.github/actions/setup$'); expect_eq "ci.yml: the web job still runs the setup action (task needs go)" 1 "$_c488a"
expect_match "ci.yml: lint's golangci-lint cache is not saved on a pull request" "skip-save-cache: \"\\$\\{\\{ github.event_name == 'pull_request' \\}\\}\"" "$(ci_job lint)"
# no workflow lets setup-node cache (its post step saves on every trigger); the pnpm store goes through the pnpm-cache action, once per setup-node
_c491a=$(count_lines -E '^\s+cache: (pnpm|npm|yarn)' "$src/.github/workflows/ci.yml" "$src/.github/workflows/release.yml" | awk -F: '{s+=$NF} END {print s+0}'); expect_eq "workflows: no setup-node cache: input"                  0 "$_c491a"
n_node=$(cat "$src/.github/workflows/ci.yml" "$src/.github/workflows/release.yml" | count_lines 'uses: actions/setup-node@')
n_pnpm=$(cat "$src/.github/workflows/ci.yml" "$src/.github/workflows/release.yml" | count_lines 'uses: ./.github/actions/pnpm-cache$')
expect_eq "workflows: every setup-node is followed by the pnpm-cache action" "$n_node" "$n_pnpm"
expect_match "docs/ci.md names the Go cache policy"             "go-cache" "$(cat "$src/docs/ci.md")"
expect_match "docs/ci.md names the pnpm-cache action"          "pnpm-cache" "$(cat "$src/docs/ci.md")"

# ── fault injection for the external commands of plan.sh / count_lines: a wrapper that prints something and then fails must make
# the script fail, never yield a plausible-looking output (#459: gates that swallowed an exit code gave a false green) ──────────
mkdir -p "$tmp/plan-bin"
printf '#!/bin/sh\necho 2026-W\nexit 5\n' > "$tmp/plan-bin/date"; chmod +x "$tmp/plan-bin/date"
rc=0; out=$(PATH="$tmp/plan-bin:$PATH" EVENT_NAME=push REF=refs/heads/main DEFAULT_BRANCH=main SCOPE=lint GITHUB_OUTPUT=/dev/stdout "$plan" 2>&1) || rc=$?
expect_eq "plan.sh: a date that prints and then fails makes plan.sh fail" "5" "$rc"
_c502a=$(printf '%s\n' "$out" | count_lines '^week='); expect_eq "plan.sh: ... and no week= line was written" 0 "$_c502a"
printf '#!/bin/sh\necho garbage\nexit 0\n' > "$tmp/plan-bin/date"
rc=0; out=$(PATH="$tmp/plan-bin:$PATH" EVENT_NAME=push REF=refs/heads/main DEFAULT_BRANCH=main SCOPE=lint GITHUB_OUTPUT=/dev/stdout "$plan" 2>&1) || rc=$?
expect_eq "plan.sh: a date that prints something that is not an ISO week makes plan.sh fail" "1" "$rc"
expect_eq "plan: plan_out reports a failing plan.sh instead of an empty value" "PLAN-FAILED" "$(PATH="$tmp/plan-bin:$PATH" plan_out push refs/heads/main main lint write)"
mkdir -p "$tmp/badgrep-bin"; printf '#!/bin/sh\necho partial\nexit 2\n' > "$tmp/badgrep-bin/grep"; chmod +x "$tmp/badgrep-bin/grep"
rc=0; (PATH="$tmp/badgrep-bin:$PATH"; echo x | count_lines x) >/dev/null 2>&1 || rc=$?
expect_eq "count_lines: a grep that fails with exit 2 stops the suite (exit 2), it is not a 0" "2" "$rc"
rc=0; out=$(echo y | count_lines x) || rc=$?
expect_eq "count_lines: no match is a 0, exit 0" "0 0" "$out $rc"

# ── go-mod-download.sh: bounded retry of the download, the last exit code passed on, no upstream text in the log (#450) ──
gmd="$src/.github/actions/setup/go-mod-download.sh"
mkdir -p "$tmp/gmd-bin"
cat > "$tmp/gmd-bin/go" <<'GOEOF'
#!/bin/sh
# fake go: fails the first $FAILS calls with exit code 7 and a marker on stderr, counts calls in $COUNT_FILE
n=$(cat "$COUNT_FILE" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "$COUNT_FILE"
if [ "$n" -le "${FAILS:-0}" ]; then echo "UPSTREAM-ERROR-TEXT proxy.golang.org says no" >&2; exit 7; fi
exit 0
GOEOF
chmod +x "$tmp/gmd-bin/go"
# gmd_run <fails> [attempts]: sets $rc, $out and $calls
gmd_run() {
  rm -f "$tmp/gmd-count"
  rc=0; out=$(PATH="$tmp/gmd-bin:$PATH" COUNT_FILE="$tmp/gmd-count" FAILS=$1 GO_MOD_DOWNLOAD_ATTEMPTS=${2:-3} GO_MOD_DOWNLOAD_SLEEP=0 "$gmd" 2>&1) || rc=$?
  calls=$(cat "$tmp/gmd-count" 2>/dev/null || echo 0)
}
gmd_run 0; expect_eq "go-mod-download: a clean download runs once"            "0 1" "$rc $calls"
gmd_run 1; expect_eq "go-mod-download: one failure is retried and succeeds"   "0 2" "$rc $calls"
gmd_run 2; expect_eq "go-mod-download: two failures are retried and succeed"  "0 3" "$rc $calls"
gmd_run 3; expect_eq "go-mod-download: three failures in a row fail, with go's own exit code" "7 3" "$rc $calls"; out3=$out
gmd_run 9; expect_eq "go-mod-download: it stops after the last attempt"        "7 3" "$rc $calls"
gmd_run 1 1; expect_eq "go-mod-download: GO_MOD_DOWNLOAD_ATTEMPTS=1 does not retry" "7 1" "$rc $calls"
expect_match "go-mod-download: the log names the attempt and the exit code" 'failed with exit code 7 \(attempt 1/3\)' "$out3"
expect_match "go-mod-download: the final line says it gave up with the last code" 'giving up after 3 attempts, last exit code 7' "$out3"
_c538a=$(printf '%s' "$out3" | count_lines 'UPSTREAM-ERROR-TEXT'); expect_eq "go-mod-download: the log never carries go's (proxy) text" 0 "$_c538a"
mkdir -p "$tmp/sleep-bin"; printf '#!/bin/sh\nexit 9\n' > "$tmp/sleep-bin/sleep"; chmod +x "$tmp/sleep-bin/sleep"
rm -f "$tmp/gmd-count"; rc=0
PATH="$tmp/sleep-bin:$tmp/gmd-bin:$PATH" COUNT_FILE="$tmp/gmd-count" FAILS=1 GO_MOD_DOWNLOAD_SLEEP=1 "$gmd" >/dev/null 2>&1 || rc=$?
expect_eq "go-mod-download: a failing sleep between attempts fails the script (it does not carry on)" "9" "$rc"

# A branch behind main must see only its own files (three-dot diff), not main's newer ones.
fixture behind
git -C "$repo" checkout -q -b change
touch_file tests/k6/x.js; commit "test: k6"
git -C "$repo" checkout -q main
touch_file pkgs/x/a.go; commit "feat: go on main"
git -C "$repo" update-ref refs/remotes/origin/main main
git -C "$repo" checkout -q change
expect_eq "branch behind main sees only its own files" "k6 k6_auth k6_wire" "$(in_repo scripts/ci-changes.sh | sed -n 's/^run_\(.*\)=true$/\1/p' | tr '\n' ' ' | sed 's/ $//')"

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
# A module file renamed out of the module still changes that module (--no-renames: both paths are listed).
affected_rename() {
  affected_fixture "$1"
  touch_file pkgs/a/x.go; commit "chore: a/x.go"
  git -C "$repo" update-ref refs/remotes/origin/main main
  git -C "$repo" checkout -q -b change
  git -C "$repo" mv pkgs/a/x.go pkgs/b/x.go
  commit "chore: move"
  in_repo scripts/affected.sh | tr '\n' ' ' | sed 's/ $//'
}
expect_eq "affected: a file moved out of a module still affects it" "pkgs/a pkgs/b" "$(affected_rename amove)"
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
  best_effort wait "$prepull_pid"   # cleanup: reaping the script we just TERMed; its status is asserted by the cases that care
  prepull_pid=""
}
trap 'test -z "$PB_PIDS" || kill $PB_PIDS 2>/dev/null; prepull_stop; rm -rf "$tmp" "$prepull_tmp"' EXIT
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
for ((_try328 = 1; _try328 <= 50; _try328++)); do [ "$(wc -l < "$prepull_tmp/pids" | tr -d ' ')" = 3 ] && break; sleep 0.1; done
expect_eq "prepull: three engines started" "3" "$(wc -l < "$prepull_tmp/pids" | tr -d ' ')"
prepull_stop
sleep 0.5
alive=0
while IFS= read -r p; do
  if kill -0 "$p" 2>/dev/null; then alive=$((alive + 1)); kill -KILL "$p" 2>/dev/null || true; fi
done < "$prepull_tmp/pids"
expect_eq "prepull: SIGTERM leaves no engine running" "0" "$alive"
rm -rf "$prepull_tmp"

# ── releasable.sh ───────────────────────────────────────────────────────────────
fixture releasable
touch_file README.md changed; touch_file docs/guide.md; touch_file pkgs/ordering/README.md
touch_file deployments/helm/x-monorepo/Chart.yaml; touch_file web/apps/console/e2e/a.spec.ts
touch_file pkgs/ordering/domain/order/order_test.go; touch_file .github/workflows/ci.yml
touch_file scripts/x.sh; touch_file Taskfile.yml; touch_file tests/k6/a.js
touch_file deployments/docker/docker-compose.yaml; touch_file deployments/docker/grafana/d.json
touch_file deployments/docker/dex/config.yaml
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
# A rename between a shipped file and a _test.go still changes what ships (--no-renames: the shipped path is listed
# either way; the pathspec already splits the pair, this pins it).
fixture relrename
touch_file pkgs/ordering/helper.go "package x"; touch_file pkgs/ordering/other_test.go "package x"
commit "chore: files at the tag"
git -C "$repo" tag -f v0.1.0 >/dev/null
git -C "$repo" mv pkgs/ordering/helper.go pkgs/ordering/helper_test.go
git -C "$repo" mv pkgs/ordering/other_test.go pkgs/ordering/other.go
commit "chore: swap"
expect_eq "renames between product and _test.go list the product path" \
  "pkgs/ordering/helper.go
pkgs/ordering/other.go" "$(in_repo scripts/releasable.sh)"
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

# ── cosign-smoke.sh ─────────────────────────────────────────────────────────────
# Against a fake cosign (no network): sign-blob writes the --bundle file, verify-blob only checks
# the bundle is there and non-empty. FAKE_SKIP_SIGN_N=<n> makes the n-th sign-blob exit 0 without
# writing anything — a cosign that "succeeds" on the equivalent-entry 409 but loses the bundle
# (#195). The smoke's three signs are: checksums.txt, then the artifact signed twice (3rd = the
# one that meets the 409). A bundle left over from the 1st sign of that artifact must not pass.
fakebin="$tmp/cosign-fake"; mkdir -p "$fakebin"
cat > "$fakebin/cosign" <<'FAKE'
#!/usr/bin/env bash
case $1 in
  version) echo "GitVersion:    vfake" ;;
  generate-key-pair) : > cosign.key; : > cosign.pub ;;
  import-key-pair) : > import-cosign.key; : > import-cosign.pub ;;
  sign-blob)
    n=$(( $(cat "$FAKE_COUNT" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$FAKE_COUNT"
    for a in "$@"; do case $a in --bundle=*) bundle=${a#--bundle=} ;; esac; done
    [ "$n" = "${FAKE_SKIP_SIGN_N:-0}" ] || echo '{"fake":"bundle"}' > "$bundle" ;;
  verify-blob)
    while [ $# -gt 0 ]; do [ "$1" = --bundle ] && bundle=$2; shift; done
    [ -s "$bundle" ] || { echo "no bundle" >&2; exit 1; } ;;
  *) exit 2 ;;
esac
FAKE
chmod +x "$fakebin/cosign"
smoke() { # smoke <skip-sign-n> -> prints "<exit> <FAIL line, if any>"
  fixture "smoke$1"; cp "$src/.goreleaser.yaml" "$repo/"
  local out rc=0
  out=$(cd "$repo" && PATH="$fakebin:$PATH" FAKE_COUNT="$tmp/count$1" FAKE_SKIP_SIGN_N=$1 scripts/cosign-smoke.sh 2>&1) || rc=$?
  first_fail=$(grep -m1 '^FAIL' <<<"$out" || [ $? -eq 1 ])
  echo "$rc $first_fail"
}
if python3 -c 'import yaml' 2>/dev/null; then
  expect_match "smoke: every sign writes a bundle passes"            '^0 $' "$(smoke 0)"
  expect_match "smoke: 2nd sign of the artifact leaves no bundle fails" '^1 FAIL: signing the same artifact again' "$(smoke 3)"
else
  echo "skip: cosign-smoke cases need python3 -c 'import yaml'"
fi

# ── cosign-sign-blob.sh ─────────────────────────────────────────────────────────
# .goreleaser.yaml's `signs.cmd` (#215 option C): cosign sign-blob, re-signed at most twice more and
# only when the failure is a Rekor 409. Fake cosign, scripted per attempt by FAKE_SEQ (space
# separated): ok = write the bundle, 409 = Rekor conflict on stderr + exit 1, err = another failure,
# empty = exit 0 with an empty bundle, none = exit 0 with no bundle. Offline, no sleeping.
signfake="$tmp/cosign-sign"; mkdir -p "$signfake"
cat > "$signfake/cosign" <<'FAKE'
#!/usr/bin/env bash
n=$(( $(cat "$FAKE_COUNT" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$FAKE_COUNT"
art=${FAKE_ART:-$3}; echo "$*" >> "$FAKE_ARGS"; echo "token=${SIGSTORE_ID_TOKEN:-}" >> "$FAKE_ARGS"
for a in "$@"; do case $a in --bundle=*) bundle=${a#--bundle=} ;; esac; done
[ -s "$bundle" ] && echo "STALE: attempt $n found a bundle left by an earlier one" >> "$FAKE_ARGS"
set -- $FAKE_SEQ; step=${!n:-err}
case $step in
  ok)    echo '{"fake":"bundle"}' > "$bundle" ;;
  409)   echo "Error: signing $art: [POST /api/v1/log/entries][409] createLogEntryConflict {\"code\":409,\"message\":\"an equivalent entry already exists\"}" >&2; echo partial > "$bundle"; exit 1 ;;
  v2)    echo "Error: signing: adding rekor v2 entry: rpc error: code = AlreadyExists desc = an equivalent entry already exists" >&2; exit 1 ;;
  nonrekor409) echo "Error: proxy: HTTP 409 Conflict from https://registry.example/v2/" >&2; exit 17 ;;
  v2http) echo "Error: signing $art: signing bundle: adding rekor v2 entry: unexpected response: 409 {\"code\":6,\"message\":\"an equivalent entry already exists with index 123\"}" >&2; exit 1 ;;
  v2503)  echo "Error: signing: adding rekor v2 entry: unexpected response: 503 {\"code\":14,\"message\":\"unavailable\"}" >&2; exit 17 ;;
  q503v2) echo "Error: signing: adding rekor v2 entry: unexpected response: 503 {\"code\":14,\"message\":\"see: adding rekor v2 entry: unexpected response: 409 {example}\"}" >&2; exit 17 ;;
  q503v2plain) echo "Error: signing: adding rekor v2 entry: unexpected response: 503 upstream said adding rekor v2 entry: unexpected response: 409 x" >&2; exit 17 ;;
  q503v1) echo "Error: signing $art: [POST /api/v1/log/entries][503] createLogEntryUnexpected {\"message\":\"see [POST /api/v1/log/entries][409] createLogEntryConflict\"}" >&2; exit 17 ;;
  q503v1plain) echo "Error: signing $art: [POST /api/v1/log/entries][503] createLogEntryUnexpected see [POST /api/v1/log/entries][409] createLogEntryConflict" >&2; exit 17 ;;
  v2brace) echo "Error: signing $art: signing bundle: error signing bundle: adding rekor v2 entry: unexpected response: 409 {\"code\":6}" >&2; exit 1 ;;
  q503multi) printf 'Error: signing /tmp/a: adding rekor v2 entry: unexpected response: 503 upstream unavailable\nError: adding rekor v2 entry: unexpected response: 409 {}\n' >&2; exit 17 ;;
  v2path503) echo "Error: signing $art: signing bundle: adding rekor v2 entry: unexpected response: 503 {}" >&2; exit 17 ;;
  v2path409) echo "Error: signing $art: signing bundle: adding rekor v2 entry: unexpected response: 409 {}" >&2; exit 1 ;;
  v2path503q) echo "Error: signing $art: signing bundle: adding rekor v2 entry: unexpected response: 503 adding rekor v2 entry: unexpected response: 409 {}" >&2; exit 17 ;;
  inlog)  echo "note: Error: signing: adding rekor v2 entry: unexpected response: 409 {}" >&2; exit 17 ;;
  mixed)  printf 'Using Rekor at https://rekor.sigstore.dev\nError: fetching signing config: HTTP 409 Conflict from https://config.example/config\n' >&2; exit 17 ;;
  url)   echo "Error: fetching signing config: GET https://config.example/409/config returned 503" >&2; exit 17 ;;
  exists) echo "Error: creating output: file AlreadyExists" >&2; exit 17 ;;
  empty) : > "$bundle" ;;
  none)  : ;;
  *)     echo "Error: signing: getting ID Token: 500 Internal Server Error" >&2; echo partial > "$bundle"; exit 1 ;;
esac
FAKE
chmod +x "$signfake/cosign"
# sign_blob <name> "<FAKE_SEQ>": runs the wrapper with goreleaser's args; sets sb_rc, sb_out, sb_tries, sb_bundle.
sign_blob() { # SB_ART=<file name> overrides the artifact name; SB_BUNDLE_ARGS (@B@ = the bundle path) the --bundle arguments
  local d="$tmp/sb-$1" art="${SB_ART:-checksums.txt}"; mkdir -p "$d"; echo artifact > "$d/$art"
  if [ -n "${SB_SENTINEL:-}" ]; then echo keep > "$d/$SB_SENTINEL"; fi
  : > "$d/args"; rm -f "$d/count"
  sb_bundle="$d/checksums.txt.sigstore.json"
  local bargs="${SB_BUNDLE_ARGS:---bundle=@B@}" bundle_args
  bargs=${bargs//@B@/$sb_bundle}; read -r -a bundle_args <<<"$bargs"
  set +e
  if [ -n "${SB_REL:-}" ]; then # a bare relative artifact name, as cosign prints it
    sb_out=$(cd "$d" && PATH="$signfake:$PATH" FAKE_COUNT="$d/count" FAKE_ARGS="$d/args" FAKE_SEQ="$2" SIGSTORE_ID_TOKEN=secret-oidc-token FAKE_ART="$art" \
      COSIGN_SIGN_RETRY_DELAY=0 "$src/scripts/cosign-sign-blob.sh" sign-blob "${bundle_args[@]}" ${SB_PRE:-} "$art" --yes 2>&1)
  else
    sb_out=$(cd "$src" && PATH="$signfake:$PATH" FAKE_COUNT="$d/count" FAKE_ARGS="$d/args" FAKE_SEQ="$2" SIGSTORE_ID_TOKEN=secret-oidc-token \
      COSIGN_SIGN_RETRY_DELAY=0 env ${SB_EXTRA:-} scripts/cosign-sign-blob.sh sign-blob "${bundle_args[@]}" "$d/$art" --yes 2>&1)
  fi
  sb_rc=$?
  set -e
  sb_tries=$(cat "$d/count" 2>/dev/null || echo 0); sb_args=$d/args
}
sign_blob first "ok"
expect_eq    "sign: first attempt succeeds"                0 "$sb_rc"
expect_eq    "sign: first attempt signs once"              1 "$sb_tries"
if [ -s "$sb_bundle" ]; then ok; else bad "sign: first attempt left no bundle"; fi
expect_match "sign: cosign gets goreleaser's args"         '^sign-blob --bundle=.*checksums.txt.sigstore.json .*checksums.txt --yes$' "$(sed -n 1p "$sb_args")"
sign_blob retry "409 ok"
expect_eq    "sign: 409 then success exits 0"              0 "$sb_rc"
expect_eq    "sign: 409 then success signs twice"          2 "$sb_tries"
if [ -s "$sb_bundle" ]; then ok; else bad "sign: 409 then success left no bundle"; fi
expect_match "sign: attempts are logged"                   'attempt 1/3' "$sb_out"
expect_match "sign: the retry is logged"                   'attempt 2/3' "$sb_out"
if grep -q STALE "$sb_args"; then bad "sign: retry started on the previous attempt's bundle"; else ok; fi
sign_blob twice "409 409 ok"
expect_eq    "sign: two 409s then success exits 0"         0 "$sb_rc"
expect_eq    "sign: two 409s then success signs 3 times"   3 "$sb_tries"
sign_blob always "409 409 409 ok"
if [ "$sb_rc" != 0 ]; then ok; else bad "sign: 409 every time must fail"; fi
expect_eq    "sign: 409 every time stops after 3 attempts" 3 "$sb_tries"
if [ ! -e "$sb_bundle" ]; then ok; else bad "sign: exhausted retries left a bundle"; fi
sign_blob other "err ok"
if [ "$sb_rc" != 0 ]; then ok; else bad "sign: non-409 error must fail"; fi
expect_eq    "sign: non-409 error is not retried"          1 "$sb_tries"
if [ ! -e "$sb_bundle" ]; then ok; else bad "sign: failed sign left a bundle"; fi
sign_blob empty "empty"
if [ "$sb_rc" != 0 ]; then ok; else bad "sign: success with an empty bundle must fail"; fi
expect_eq    "sign: empty bundle is not retried"           1 "$sb_tries"
if [ ! -s "$sb_bundle" ]; then ok; else bad "sign: empty bundle case left a non-empty bundle"; fi
sign_blob none "none"
if [ "$sb_rc" != 0 ]; then ok; else bad "sign: success with no bundle must fail"; fi
sign_blob v2 "v2 v2 v2"
expect_eq    "sign: a gRPC-shaped AlreadyExists (cosign never prints one) is not retried" 1 "$sb_tries"
# The pinned cosign v3.1.4 (sigstore-go v1.3.0 + rekor-tiles v2.3.0 pkg/client/write/write.go) words a
# Rekor v2 409 as "adding rekor v2 entry: unexpected response: 409 <json body>" (observed with the real
# cosign against a loopback v2 server, PR #249 review).
sign_blob v2http "v2http ok"
expect_eq    "sign: Rekor v2 HTTP 409 (unexpected response) is retried" 2 "$sb_tries"
expect_eq    "sign: Rekor v2 HTTP 409 then success"          0 "$sb_rc"
# The artifact path is part of the error line: strip it before reading the status (#256).
odd='a{b] adding rekor v2 entry: unexpected response: 409.txt'
SB_ART=$odd sign_blob v2path503 "v2path503 v2path503 v2path503"
expect_eq    "sign: a 503 for an artifact named like a 409 is not retried" 1 "$sb_tries"
SB_ART=$odd sign_blob v2path409 "v2path409 ok"
expect_eq    "sign: a real 409 for an odd artifact name is retried" 2 "$sb_tries"
# Names that overlap the fixed error text, or the status itself, must not change how it is read.
for nm in entry a v2 api; do
  SB_REL=1 SB_ART=$nm sign_blob "short-$nm" "v2path409 ok"
  expect_eq  "sign: a real v2 409 for an artifact named '$nm' is retried" 2 "$sb_tries"
done
SB_REL=1 SB_ART='503 ' sign_blob short-503sp "v2path503q v2path503q v2path503q"
expect_eq    "sign: a 503 quoting a 409, artifact named '503 ', is not retried" 1 "$sb_tries"
# The subcommand is an argument too, but not the artifact: a file named like `sign-blob: <marker>` must not
# be taken for the context prefix (PR #259 review).
SB_REL=1 SB_ART='sign-blob: adding rekor v2 entry: unexpected response: 503 x' sign_blob subcmd-409 "v2path409 ok"
expect_eq    "sign: a real 409, artifact named 'sign-blob: ...503 x', is retried" 2 "$sb_tries"
SB_REL=1 SB_ART='sign-blob: adding rekor v2 entry: unexpected response: 409 x' sign_blob subcmd-503 "v2path503 v2path503 v2path503"
expect_eq    "sign: a 503, artifact named 'sign-blob: ...409 x', is not retried" 1 "$sb_tries"
# A flag the wrapper does not know (#260, was: one attempt without classification, PR #259): cosign
# may take the next token as its value, so the wrapper cannot tell the artifact or the bundle path.
# Usage error, exit 2, before cosign runs; whatever the file name says, a real 409 included.
odd409='adding rekor v2 entry: unexpected response: 409 artifact'
unknown_flag_refused() { # unknown_flag_refused <name> <label>: after SB_PRE / SB_ART / FAKE_SEQ are set by the caller
  expect_eq    "sign: $2 exits 2"            2 "$sb_rc"
  expect_eq    "sign: $2 never calls cosign" 0 "$sb_tries"
}
SB_REL=1 SB_PRE='--oidc-client-id fixture-client' SB_ART=$odd409 sign_blob unknownflag-503 "v2path503 v2path503 v2path503"
unknown_flag_refused unknownflag-503 "an unknown flag next to an artifact named like a 409"
# The flag value equals the start of the file name: a string-prefix match cannot prove the artifact was
# identified (PR #259 r5).
SB_REL=1 SB_PRE='--oidc-client-id fixture-client' SB_ART='fixture-client: adding rekor v2 entry: unexpected response: 409 artifact' sign_blob unknownflag-collide "v2path503 v2path503 v2path503"
unknown_flag_refused unknownflag-collide "an unknown flag whose value prefixes the file name"
SB_REL=1 SB_PRE='--oidc-client-id fixture-client' sign_blob unknownflag-409 "v2path409 ok"
unknown_flag_refused unknownflag-409 "an unknown flag on a would-be real 409"
# The argv whitelist (parse_argv): known flags keep the retry; `--` and a positional count other than one
# keep the bundle path unambiguous, so they run once, unclassified (#260); unknown flags are the cases above.
SB_REL=1 SB_PRE='-y' sign_blob argv-y "v2path409 ok"
expect_eq    "sign: -y is a known flag, a real 409 is still retried" 2 "$sb_tries"
SB_REL=1 SB_PRE='--' sign_blob argv-dashdash "v2path409 ok"
expect_eq    "sign: -- is understood but names no artifact: one attempt, no retry" 1 "$sb_tries"
SB_REL=1 SB_PRE='second-positional' sign_blob argv-two "v2path409 ok"
expect_eq    "sign: two positionals name no artifact: one attempt, no retry" 1 "$sb_tries"
# One parser, one bundle path (PR #259 r6): a --bundle the wrapper cannot pin down is a usage error
# (exit 2) before cosign runs, never a retry with a stale or unknown bundle.
bundle_usage() { # bundle_usage <name> "<--bundle args, @B@ = bundle path>" [extra args]
  SB_BUNDLE_ARGS=$2 sign_blob "$1" "409 409 409"
  expect_eq    "sign: $1 exits 2" 2 "$sb_rc"
  expect_eq    "sign: $1 never calls cosign" 0 "$sb_tries"
  if [ ! -e "$sb_bundle" ] && [ ! -e "$sb_bundle.old" ]; then ok; else bad "sign: $1 left a bundle"; fi
}
bundle_usage bundle-twice-old-first "--bundle=@B@.old --bundle=@B@"
bundle_usage bundle-twice-new-first "--bundle=@B@ --bundle=@B@.old"
bundle_usage bundle-separate        "--bundle @B@"
bundle_usage bundle-empty           "--bundle="
# After `--` everything is a positional: a file named `--bundle=decoy` is the artifact, never the bundle,
# so the unrelated file `decoy` is not removed and, as `--` names no artifact, there is one attempt.
SB_REL=1 SB_PRE='--' SB_ART='--bundle=decoy' SB_SENTINEL=decoy sign_blob dashdash-decoy "v2path409 ok"
expect_eq    "sign: after -- a --bundle=... artifact makes exactly one attempt" 1 "$sb_tries"
if [ -e "$tmp/sb-dashdash-decoy/decoy" ]; then ok; else bad "sign: the file named by a --bundle=decoy artifact after -- was deleted"; fi
# An argv the wrapper cannot interpret never reaches cosign and never touches a file (#260): cosign
# gives `--oidc-client-id` the next token as its value, so in `--oidc-client-id --bundle=decoy` there
# is no bundle at all and `decoy` is not the wrapper's to remove. Usage error, exit 2, 0 cosign calls.
sign_argv_refused() { # sign_argv_refused <name> <args...>: runs the wrapper in a tempdir holding the sentinel `decoy`
  local n=$1 d="$tmp/sb-$1"; shift; mkdir -p "$d"; echo artifact > "$d/artifact"; echo keep > "$d/decoy"
  : > "$d/args"; rm -f "$d/count"
  set +e
  sb_out=$(cd "$d" && PATH="$signfake:$PATH" FAKE_COUNT="$d/count" FAKE_ARGS="$d/args" FAKE_SEQ="err" \
    COSIGN_SIGN_RETRY_DELAY=0 "$src/scripts/cosign-sign-blob.sh" "$@" 2>&1)
  sb_rc=$?
  set -e
  sb_tries=$(cat "$d/count" 2>/dev/null || echo 0)
  expect_eq    "sign: $n exits 2"                 2 "$sb_rc"
  expect_eq    "sign: $n never calls cosign"      0 "$sb_tries"
  if [ "$(cat "$d/decoy" 2>/dev/null)" = keep ]; then ok; else bad "sign: $n removed or changed the sentinel file"; fi
  # No diagnostic echoes an argv value: an unknown flag's value may be a secret (--identity-token).
  case $sb_out in *"$canary"*) bad "sign: $n printed an argv value: $sb_out" ;; *) ok ;; esac
}
canary=synthetic-pr261-token-canary-DO-NOT-LOG
sign_argv_refused unknown-value-flag-swallows-bundle sign-blob --oidc-client-id --bundle=decoy --yes --key KEY --signing-config CFG artifact
sign_argv_refused unknown-flag-after-bundle sign-blob --bundle=decoy --oidc-client-id x artifact --yes
sign_argv_refused other-subcommand verify-blob --bundle=decoy artifact
sign_argv_refused canary-flag-equals sign-blob --bundle=decoy "--identity-token=$canary" artifact --yes
sign_argv_refused canary-flag-separate sign-blob --bundle=decoy --identity-token "$canary" artifact --yes
sign_argv_refused canary-bare-after-unknown sign-blob --bundle=decoy --frobnicate "$canary" artifact --yes
sign_argv_refused canary-dash-token sign-blob --bundle=decoy "-s$canary" artifact --yes
sign_argv_refused canary-subcommand "$canary" --bundle=decoy artifact
sign_argv_refused canary-key-missing-value sign-blob --bundle=decoy artifact "--key"
SB_PRE='--oidc-client-id fixture-client' bundle_usage bundle-twice-unknown-flag "--bundle=@B@.old --bundle=@B@"
SB_ART='artifact{build}.txt' sign_blob v2brace "v2brace ok"
expect_eq    "sign: a real v2 409 is retried even when the artifact path has a brace" 2 "$sb_tries"
sign_blob v2503 "v2503 v2503 v2503"
expect_eq    "sign: Rekor v2 HTTP 503 is not retried"        1 "$sb_tries"
# Anchored to the outer error line (#256): a non-conflict whose body quotes a 409 is one call.
for q in q503v2 q503v2plain q503v1 q503v1plain inlog q503multi; do
  sign_blob "$q" "$q $q $q"
  expect_eq    "sign: $q (409 text inside another status's line) is not retried" 1 "$sb_tries"
  if [ "$sb_rc" != 0 ]; then ok; else bad "sign: $q must fail"; fi
done
sign_blob mixed "mixed mixed mixed"
expect_eq    "sign: a 409 from another service next to a Rekor line is not retried" 1 "$sb_tries"
sign_blob url "url url url"
expect_eq    "sign: a /409/ in a URL is not a conflict"      1 "$sb_tries"
if [ "$sb_rc" != 0 ]; then ok; else bad "sign: URL-409 failure must fail"; fi
sign_blob exists "exists exists exists"
expect_eq    "sign: AlreadyExists outside Rekor is not retried" 1 "$sb_tries"
SB_EXTRA="COSIGN_SIGN_RETRIES=3" sign_blob ceiling "409 409 409 409 ok"
if [ "$sb_rc" != 0 ]; then ok; else bad "sign: COSIGN_SIGN_RETRIES must not raise the ceiling"; fi
expect_eq    "sign: the ceiling stays 3 attempts"            3 "$sb_tries"
for rv in -1 abc ""; do
  SB_EXTRA="COSIGN_SIGN_RETRIES=$rv" sign_blob "ceiling$rv" "409 409 409 409 ok"
  expect_eq  "sign: COSIGN_SIGN_RETRIES='$rv' is ignored, ceiling stays 3" 3 "$sb_tries"
done
sign_blob nonrekor409 "nonrekor409 ok"
expect_eq    "sign: a 409 that does not name Rekor is not retried" 1 "$sb_tries"
if [ "$sb_rc" != 0 ]; then ok; else bad "sign: non-Rekor 409 must fail"; fi
badseq="$tmp/badseq"; mkdir -p "$badseq"
printf '#!/bin/sh\necho seq: broken >&2\nexit 127\n' > "$badseq/seq"; chmod +x "$badseq/seq"
SB_EXTRA="PATH=$badseq:$signfake:$PATH" sign_blob noseq "ok"
expect_eq    "sign: a broken seq does not skip the signing"  1 "$sb_tries"
expect_eq    "sign: a broken seq still signs and exits 0"    0 "$sb_rc"
if [ -s "$sb_bundle" ]; then ok; else bad "sign: broken seq left no bundle"; fi
SB_EXTRA="COSIGN_SIGN_RETRY_DELAY=abc" sign_blob baddelay "409 ok"
if [ "$sb_rc" != 0 ]; then ok; else bad "sign: a non-numeric delay must fail"; fi
sign_blob token "409 409 409"
case $sb_out in *secret-oidc-token*) bad "sign: the OIDC token reached the log" ;; *) ok ;; esac

# ── rekor-v2-canary.sh ──────────────────────────────────────────────────────────
# Fails when the default signing config of the installed cosign lists a Rekor that is not v1 (#215):
# #204's 409 handling (cosign v3.1.4 / sigstore-go v1.3.0) exists for Rekor v1 only. Offline: the
# config comes from REKOR_CANARY_SIGNING_CONFIG, or from a fake cosign whose `initialize` writes one
# into TUF_ROOT the way the real one does. The positive case against the real default config is the
# CI job itself.
canary_cfg() { # canary_cfg <name> <rekorTlogUrls json> -> path of a signing config with those logs
  local f="$tmp/canary-$1.json"
  printf '{"mediaType":"application/vnd.dev.sigstore.signingconfig.v0.2+json","rekorTlogUrls":%s,"rekorTlogConfig":{"selector":"ANY"}}\n' "$2" > "$f"
  echo "$f"
}
canary() { # canary <config path> -> "<exit> <output>"
  local out rc=0
  out=$(REKOR_CANARY_SIGNING_CONFIG=$1 "$src/scripts/rekor-v2-canary.sh" 2>&1) || rc=$?
  echo "$rc $out"
}
v1='{"url":"https://rekor.sigstore.dev","majorApiVersion":1,"validFor":{"start":"2021-01-12T11:53:27.000Z"},"operator":"sigstore.dev"}'
v2='{"url":"https://log2025-1.rekor.sigstore.dev","majorApiVersion":2,"validFor":{"start":"2025-10-01T00:00:00Z"},"operator":"sigstore.dev"}'
if command -v jq >/dev/null; then
  expect_match "canary: v1 only passes"                  '^0 '                     "$(canary "$(canary_cfg v1 "[$v1]")")"
  expect_match "canary: a v2 log fails"                  '^1 '                     "$(canary "$(canary_cfg v2 "[$v2]")")"
  expect_match "canary: v2 failure points at #215"       '#215'                    "$(canary "$(canary_cfg v2b "[$v2]")")"
  expect_match "canary: v2 failure names the log"        'log2025-1\.rekor'        "$(canary "$(canary_cfg v2c "[$v2]")")"
  expect_match "canary: v1 + v2 (selector ANY) fails"    '^1 '                     "$(canary "$(canary_cfg both "[$v1,$v2]")")"
  expect_match "canary: unknown major version fails"     '^1 '                     "$(canary "$(canary_cfg v3 "[$v1,${v2/\"majorApiVersion\":2/\"majorApiVersion\":3}]")")"
  expect_match "canary: no rekor log at all fails"       '^1 '                     "$(canary "$(canary_cfg none "[]")")"
  echo 'not json' > "$tmp/canary-garbage.json"
  expect_match "canary: unparseable config fails closed" '^1 '                     "$(canary "$tmp/canary-garbage.json")"
  expect_match "canary: missing config file fails closed" '^1 '                    "$(canary "$tmp/canary-absent.json")"
  # cosign initialize only warns when it cannot fetch the signing config, so what lands in TUF_ROOT
  # may be empty or the wrong shape: jq reads zero inputs without an error, and that must not pass.
  : > "$tmp/canary-empty.json"; printf ' \n\t\n' > "$tmp/canary-blank.json"
  echo '{}' > "$tmp/canary-noarray.json"; echo '{"rekorTlogUrls":"https://rekor.sigstore.dev"}' > "$tmp/canary-str.json"
  echo '{"rekorTlogUrls":["https://rekor.sigstore.dev"]}' > "$tmp/canary-strentry.json"
  { echo "{\"rekorTlogUrls\":[$v1]}"; echo "{\"rekorTlogUrls\":[$v1]}"; } > "$tmp/canary-two.json"
  expect_match "canary: empty file fails closed"          '^1 '                     "$(canary "$tmp/canary-empty.json")"
  expect_match "canary: whitespace-only file fails closed" '^1 '                    "$(canary "$tmp/canary-blank.json")"
  expect_match "canary: {} fails closed"                  '^1 '                     "$(canary "$tmp/canary-noarray.json")"
  expect_match "canary: rekorTlogUrls not an array fails" '^1 '                     "$(canary "$tmp/canary-str.json")"
  expect_match "canary: non-object log entry fails"       '^1 [^ ]'                 "$(canary "$tmp/canary-strentry.json")"
  expect_match "canary: two JSON documents fail"          '^1 '                     "$(canary "$tmp/canary-two.json")"
  expect_match "canary: empty file says unreadable, not ok" 'FAIL' "$(canary "$tmp/canary-empty.json")"
  # The rest of #235's matrix: a document that parses but cannot name the log it describes.
  printf 'null\n' > "$tmp/canary-null.json"; printf '{"rekorTlogUrls":[{"url":"https://rekor.sig' > "$tmp/canary-trunc.json"
  echo '{"rekorTlogUrls":[{"url":"","majorApiVersion":1}]}' > "$tmp/canary-emptyurl.json"
  echo '{"rekorTlogUrls":[{"majorApiVersion":1}]}' > "$tmp/canary-nourl.json"
  echo '{"rekorTlogUrls":[{"url":5,"majorApiVersion":1}]}' > "$tmp/canary-numurl.json"
  expect_match "canary: JSON null fails closed"           '^1 '                     "$(canary "$tmp/canary-null.json")"
  expect_match "canary: truncated JSON fails closed"      '^1 '                     "$(canary "$tmp/canary-trunc.json")"
  expect_match "canary: log with an empty url fails"      '^1 '                     "$(canary "$tmp/canary-emptyurl.json")"
  expect_match "canary: log without a url fails"          '^1 '                     "$(canary "$tmp/canary-nourl.json")"
  expect_match "canary: log with a non-string url fails"  '^1 '                     "$(canary "$tmp/canary-numurl.json")"

  # Resolution through cosign: a fake `initialize` writes the config under TUF_ROOT/<mirror>/targets/.
  canary_fake="$tmp/cosign-canary"; mkdir -p "$canary_fake"
  cat > "$canary_fake/cosign" <<'FAKE'
#!/usr/bin/env bash
[ "$1" = initialize ] || exit 2
[ "${FAKE_TUF_DOWN:-}" = 1 ] && { echo "tuf: dial tcp: lookup tuf-repo-cdn.sigstore.dev: no such host" >&2; exit 1; }
[ -n "${FAKE_CONFIG:-}" ] || exit 0   # initialised, but no signing config among the targets
mkdir -p "$TUF_ROOT/tuf-repo-cdn.example/targets"
cp "$FAKE_CONFIG" "$TUF_ROOT/tuf-repo-cdn.example/targets/signing_config.v0.2.json"
FAKE
  chmod +x "$canary_fake/cosign"
  canary_via() { # canary_via <env assignment> -> "<exit> <output>"
    local out rc=0
    out=$(env -u REKOR_CANARY_SIGNING_CONFIG PATH="$canary_fake:$PATH" REKOR_CANARY_RETRY_DELAY=0 "$@" "$src/scripts/rekor-v2-canary.sh" 2>&1) || rc=$?
    echo "$rc $out"
  }
  expect_match "canary via cosign: v1 default passes"   '^0 '        "$(canary_via FAKE_CONFIG="$(canary_cfg viav1 "[$v1]")")"
  expect_match "canary via cosign: v2 default fails"    '^1 '        "$(canary_via FAKE_CONFIG="$(canary_cfg viav2 "[$v1,$v2]")")"
  expect_match "canary via cosign: no signing config fails" '^1 '   "$(canary_via FAKE_CONFIG=)"
  expect_match "canary via cosign: empty signing config target fails" '^1 ' "$(canary_via FAKE_CONFIG="$tmp/canary-empty.json")"
  expect_match "canary via cosign: whitespace target fails"  '^1 ' "$(canary_via FAKE_CONFIG="$tmp/canary-blank.json")"
  tuf_down=$(canary_via FAKE_TUF_DOWN=1)
  expect_match "canary via cosign: TUF unreachable fails closed" '^1 ' "$tuf_down"
  expect_match "canary via cosign: TUF outage is not called v2"  'TUF|initialize' "$tuf_down"
  if printf '%s' "$tuf_down" | grep -q 'majorApiVersion'; then bad "TUF outage reported as a v2 finding: [$tuf_down]"; else ok; fi
else
  echo "skip: rekor-v2-canary cases need jq"
fi

# ── red-stage.sh ────────────────────────────────────────────────────────────────
# Classifies `go test` output of a RED run: compile / panic / assertion / setup / pass. Exit 0 only
# for `assertion` (the expected message was printed under a --- FAIL).
red() { local rc=0 out; out=$(printf '%s\n' "$2" | scripts/red-stage.sh -m "$1") || rc=$?; echo "$out $rc"; }
want_msg='want 1 call.* got 0'
expect_eq "red: target assertion"  "assertion 0" "$(red "$want_msg" '=== RUN   TestCreate
    order_test.go:42: find calls: want 1 call, got 0
--- FAIL: TestCreate (0.00s)
FAIL
FAIL	example.com/x/ordering	0.012s')"
expect_eq "red: setup failed before the target" "setup 1" "$(red "$want_msg" '=== RUN   TestCreate
    order_test.go:20: seed CreateOrder: invalid customer
--- FAIL: TestCreate (0.00s)
FAIL
FAIL	example.com/x/ordering	0.012s')"
expect_eq "red: compile failure" "compile 1" "$(red "$want_msg" '# example.com/x/ordering [example.com/x/ordering.test]
./order_test.go:9:14: undefined: NewThing
FAIL	example.com/x/ordering [build failed]')"
expect_eq "red: unrelated typo is still compile" "compile 1" "$(red "$want_msg" '# example.com/x/ordering
./order.go:3:1: syntax error: unexpected }
FAIL	example.com/x/ordering [build failed]
FAIL')"
expect_eq "red: panic before the assertion" "panic 1" "$(red "$want_msg" '--- FAIL: TestCreate (0.00s)
panic: runtime error: invalid memory address or nil pointer dereference [recovered]
FAIL	example.com/x/ordering	0.012s')"
expect_eq "red: green run is not a RED" "pass 1" "$(red "$want_msg" 'ok  	example.com/x/ordering	0.012s')"
expect_eq "red: message only in the RUN echo is not an assertion" "setup 1" "$(red "$want_msg" '=== RUN   want 1 call, got 0
--- FAIL: TestCreate (0.00s)
FAIL')"
# A long log must not flip a found message to setup: grep -q exiting early SIGPIPEs the filter
# upstream of it, and pipefail turns that into "not found".
long_log=$(printf '=== RUN   TestX\n    x_test.go:10: target assertion\n'; for ((_try801 = 1; _try801 <= 20000; _try801++)); do printf '    long diagnostic line %0100d\n' 0; done; printf -- '--- FAIL: TestX (0.00s)\nFAIL\n')
expect_eq "red: long output still assertion" "assertion 0" "$(out=$(printf '%s\n' "$long_log" | scripts/red-stage.sh -m 'target assertion') && rc=0 || rc=$?; echo "$out $rc")"
set +e; scripts/red-stage.sh </dev/null >/dev/null 2>&1; rc=$?; set -e
expect_eq "red: -m is required" 64 "$rc"

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

# The title must match exactly: a substring match let "...base=<main>-wrong-tree" through.
for bad_title in "merge-tree pr=7 head=$head_sha base=$main_sha-wrong-tree" \
                 "merge-tree pr=7 head=$head_sha base=${main_sha}0" \
                 "merge-tree pr=7 head=${head_sha}0 base=$main_sha" \
                 "merge-tree pr=77 head=$head_sha base=$main_sha" \
                 "x merge-tree pr=7 head=$head_sha base=$main_sha" \
                 "merge-tree pr=7 head=$head_sha base=$main_sha x"; do
  printf '78 completed success %s\n' "$bad_title" > "$tmp/stub-stale/tree_runs"; ready stale
  expect_eq "inexact merge-tree title [$bad_title] exits 2" 2 "$rc"
done
printf '79 in_progress  merge-tree pr=7 head=%s base=%s\n' "$head_sha" "$main_sha" > "$tmp/stub-stale/tree_runs"; ready stale
expect_eq    "unfinished merge-tree run does not count" 2 "$rc"
printf '80 completed failure merge-tree pr=7 head=%s base=%s\n' "$head_sha" "$main_sha" > "$tmp/stub-stale/tree_runs"; ready stale
expect_eq    "failed merge-tree run does not count" 2 "$rc"
printf '81 completed success merge-tree pr=7 head=%s base=%s\n' "$head_sha" "$main_sha" > "$tmp/stub-stale/tree_runs"; ready stale
expect_eq    "exact merge-tree title still counts" 0 "$rc"

# ── merge-tree-inputs.sh: the workflow's first step; the run title carries these values ──
inputs_rc() { set +e; "$src/scripts/merge-tree-inputs.sh" "$@" >/dev/null 2>&1; local r=$?; set -e; echo "$r"; }
expect_eq "inputs: valid pr / head / base accepted"  0 "$(inputs_rc 7 "$head_sha" "$main_sha")"
expect_eq "inputs: non-numeric pr rejected"          1 "$(inputs_rc 7a "$head_sha" "$main_sha")"
expect_eq "inputs: empty pr rejected"                1 "$(inputs_rc '' "$head_sha" "$main_sha")"
expect_eq "inputs: short head rejected"              1 "$(inputs_rc 7 "${head_sha%?}" "$main_sha")"
expect_eq "inputs: long base rejected"               1 "$(inputs_rc 7 "$head_sha" "${main_sha}0")"
expect_eq "inputs: uppercase head rejected"          1 "$(inputs_rc 7 "$(printf '%s' "$head_sha" | tr 0-9 A-J)" "$main_sha")"
expect_eq "inputs: non-hex base rejected"            1 "$(inputs_rc 7 "$head_sha" "${main_sha%?}g")"
expect_eq "inputs: head with suffix injection rejected" 1 "$(inputs_rc 7 "$head_sha x" "$main_sha")"
expect_eq "inputs: base with newline suffix rejected"   1 "$(inputs_rc 7 "$head_sha" "$main_sha
x")"
expect_eq "inputs: pr with suffix injection rejected"   1 "$(inputs_rc '7 x' "$head_sha" "$main_sha")"
expect_eq "inputs: wrong arg count is a usage error" 3 "$(inputs_rc 7 "$head_sha")"

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

# ── deployments/helm/kind/coredns-dex.sh ──────────────────────────────────────
# Run the real script against a stub kubectl whose "cluster" is one Corefile file; the patch the
# script sends is written back, so every case sees what a real CoreDNS would be given.
dns_dir="$tmp/coredns"; mkdir -p "$dns_dir/bin"
cat > "$dns_dir/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case " $* " in
  *" get configmap coredns "*) cat "$COREFILE" ;;
  *" patch configmap coredns "*) jq -j '.data.Corefile' <<<"${*: -1}" > "$COREFILE"; echo patch >> "$KLOG" ;;
  *" rollout "*) echo rollout >> "$KLOG" ;;
  *) echo "stub kubectl: unexpected $*" >&2; exit 1 ;;
esac
STUB
chmod +x "$dns_dir/bin/kubectl"
# dns <namespace> : runs the script, prints "<exit> <patches>"; the Corefile is left in $dns_dir/Corefile
dns() {
  : > "$dns_dir/klog"
  local rc=0
  mkdir -p "$dns_dir/home"; : > "$dns_dir/home/.kind-runner"  # the script refuses to start outside run.sh's scratch HOME (#444)
  HOME="$dns_dir/home" KIND_RUNNER_HOME="$dns_dir/home" COREFILE="$dns_dir/Corefile" KLOG="$dns_dir/klog" PATH="$dns_dir/bin:$PATH" "$src/deployments/helm/kind/coredns-dex.sh" "$1" >/dev/null 2>&1 || rc=$?
  n_patch=$(grep -c '^patch$' "$dns_dir/klog" || [ $? -eq 1 ])
  echo "$rc $n_patch"
}
corefile_with() { # corefile_with <extra lines after ready...>
  { printf '.:53 {\n    errors\n    ready\n'; for l in "$@"; do printf '    %s\n' "$l"; done
    printf '    kubernetes cluster.local in-addr.arpa ip6.arpa {\n        pods insecure\n    }\n    forward . /etc/resolv.conf\n}\n'; } > "$dns_dir/Corefile"
}
rule_shop="rewrite name exact dex.localtest.me dex.shop.svc.cluster.local"
corefile_with
expect_eq "coredns: first run patches once"        "0 1" "$(dns shop)"
expect_eq "coredns: rule lands after ready"        "ready $rule_shop" "$(grep -A1 '^ *ready$' "$dns_dir/Corefile" | tr -s ' \n' ' ' | sed 's/^ *//;s/ *$//;s/^ready  */ready /')"
expect_eq "coredns: second run is a no-op"         "0 0" "$(dns shop)"
expect_eq "coredns: namespace change replaces"     "0 1" "$(dns other)"
_c1308a=$(count_lines 'exact dex.localtest.me ' "$dns_dir/Corefile"); expect_eq "coredns: exactly one dex rule remains"  "1" "$_c1308a"
_c1309a=$(count_lines 'dex.other.svc.cluster.local' "$dns_dir/Corefile"); expect_eq "coredns: new target is in place"        "1" "$_c1309a"
corefile_with "rewrite name exact dex.localtest.me.backup backup.shop.svc.cluster.local" "rewrite name exact other.localtest.me o.shop.svc.cluster.local"
expect_eq "coredns: similar names get their own rule" "0 1" "$(dns shop)"
_c1312a=$(count_lines 'dex.localtest.me.backup backup.shop' "$dns_dir/Corefile"); expect_eq "coredns: similar-name rewrite survives"  "1" "$_c1312a"
_c1313a=$(count_lines 'other.localtest.me o.shop' "$dns_dir/Corefile"); expect_eq "coredns: unrelated rewrite survives"     "1" "$_c1313a"
_c1314a=$(count_lines "^    $rule_shop\$" "$dns_dir/Corefile"); expect_eq "coredns: our rule added beside them"     "1" "$_c1314a"
corefile_with "# $rule_shop"
expect_eq "coredns: a commented rule is not the rule" "0 1" "$(dns shop)"
_c1317a=$(count_lines "^    # $rule_shop\$" "$dns_dir/Corefile"); expect_eq "coredns: the comment is kept"            "1" "$_c1317a"
_c1318a=$(count_lines "^    $rule_shop\$" "$dns_dir/Corefile"); expect_eq "coredns: and a live rule was added"      "1" "$_c1318a"
printf '.:53 {\n    errors\n}\n' > "$dns_dir/Corefile"
expect_eq "coredns: no ready line refuses, no patch" "1 0" "$(dns shop)"

# ── k6-bench-verdict.sh: bench.sh's exit code → the verdict of a k6-bench run (#280) ──
kv() { # <rc> → "<script exit> <verdict.txt>" for a fresh out dir
  local d; d=$(mktemp -d -p "$tmp"); local rc=0
  "$src/scripts/k6-bench-verdict.sh" "$1" "$d" >/dev/null 2>&1 || rc=$?
  echo "$rc $(cat "$d/verdict.txt")"
}
expect_match "k6-bench: exit 0 is the only valid one"        '^0 VALID'                                "$(kv 0)"
expect_match "k6-bench: exit 3 is NOT A VALID BASELINE, red" '^1 INVALID: NOT A VALID BASELINE'        "$(kv 3)"
expect_match "k6-bench: exit 1 is a failed bench, red"       '^1 INVALID: bench.sh failed \(exit 1\)'  "$(kv 1)"
expect_match "k6-bench: exit 42 keeps its code"              '^1 INVALID: bench.sh failed \(exit 42\)' "$(kv 42)"
expect_match "k6-bench: exit 99 (correctness threshold) is a failed bench, red" '^1 INVALID: bench.sh failed \(exit 99\): a k6 threshold that is not overload' "$(kv 99)"
expect_match "k6-bench: Task's own 201 is a failed bench"    '^1 INVALID: bench.sh failed \(exit 201\)' "$(kv 201)"
expect_match "k6-bench: no exit code = did not finish, red"  '^1 INVALID: the bench step did not finish' "$(kv '')"
# the same thing against the real Task: a failing command is 201 unless --exit-code (skipped without task on PATH)
if command -v task >/dev/null 2>&1; then
  tf=$(mktemp -d -p "$tmp"); printf 'version: "3"\ntasks:\n  t:\n    cmds: ["exit 3"]\n' > "$tf/Taskfile.yml"
  plain=0; (cd "$tf" && task t >/dev/null 2>&1) || plain=$?
  kept=0;  (cd "$tf" && task --exit-code t >/dev/null 2>&1) || kept=$?
  expect_eq "task without --exit-code hides bench.sh's 3"  "201" "$plain"
  expect_eq "task --exit-code hands bench.sh's 3 through" "3"   "$kept"
fi
# Task 3.x maps a failing command to 201 unless --exit-code; the workflow must pass it or 3 never reaches the verdict
if grep -Eq 'task --exit-code k6-bench' "$src/.github/workflows/k6-bench.yml"; then ok; else bad "k6-bench.yml must run task --exit-code k6-bench"; fi
if grep -Eq 'scripts/k6-bench-verdict.sh' "$src/.github/workflows/k6-bench.yml"; then ok; else bad "k6-bench.yml must classify with scripts/k6-bench-verdict.sh"; fi

# ── scripts/dex-prebind.sh ───────────────────────────────────────────────────
# Against tests/dex-prebind/stub_rpc.py (an in-memory IdentityService) and a fake dex-token.sh: the
# first run binds both Dex accounts through CreateAccount → CreateInvite → ClaimInvite, a rerun
# changes nothing, a non-dev issuer or a non-local server is refused before any request, and
# neither the API key nor a token reaches the output (#274 part 2).
pb_key="xk_stub-admin-secret-9f3a"
pb_dev=http://dex.localtest.me:5556/dex
pb_fake="$tmp/fake-dex-token.sh"
cat > "$pb_fake" <<'FAKE'
#!/usr/bin/env bash
echo "$1" >> "$PB_TOKEN_CALLS"
if [ -n "${PB_TOKEN_PLAIN:-}" ]; then echo "tokplain$(printf %s "$1" | tr -c 'a-z0-9' x)"; else echo "tok-secret-$1"; fi
FAKE
chmod +x "$pb_fake"
# pb_up <name> [VAR=value...]: starts a stub (extra env for it); sets pb_dir, pb_url.
pb_up() {
  pb_dir="$tmp/pb-$1"; mkdir -p "$pb_dir"; shift
  rm -f "$pb_dir/port"; : > "$pb_dir/log"; : > "$pb_dir/tokens"   # a stale port file would point at the previous stub of the same name
  env STUB_ADMIN_KEY="$pb_key" STUB_ISSUER="$pb_dev" "$@" python3 "$src/tests/dex-prebind/stub_rpc.py" "$pb_dir/port" "$pb_dir/log" >/dev/null 2>&1 &
  PB_PIDS="$PB_PIDS $!"
  local i=0; while [ ! -s "$pb_dir/port" ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
  pb_url="http://127.0.0.1:$(cat "$pb_dir/port")"
}
# pb_run [VAR=value...]: runs the script against the current stub; sets out (stdout+stderr) and rc.
pb_run() {
  rc=0
  out=$(env ADMIN_API_KEY="$pb_key" BASE_URL="$pb_url" DEX_TOKEN_CMD="$pb_fake" PB_TOKEN_CALLS="$pb_dir/tokens" "$@" "$src/scripts/dex-prebind.sh" 2>&1) || rc=$?
}
pb_state() { curl -fsS "$pb_url/_state"; }
pb_calls() { pb_n=$(grep -c "^$1 " "$pb_dir/log" || [ $? -eq 1 ]); }   # sets $pb_n: an assignment aborts on a grep error, "$(pb_calls …)" in an argument would not

pb_up first
pb_run
expect_eq "dex-prebind: first run succeeds" "0" "$rc"
st=$(pb_state)
expect_eq "dex-prebind: admin account has the admin role" '["admin"]' "$(echo "$st" | jq -c '[.accounts[] | select(.name=="admin@example.com") | .roles[]]')"
expect_eq "dex-prebind: user account has no role"         '[]'        "$(echo "$st" | jq -c '[.accounts[] | select(.name=="user@example.com") | .roles[]]')"
expect_eq "dex-prebind: two accounts, two claimed invites, two bindings" "2 2 2" \
  "$(echo "$st" | jq -r '[(.accounts|length), ([.invites[]|select(.status=="INVITE_STATUS_CLAIMED")]|length), (.identities|length)] | join(" ")')"
expect_eq "dex-prebind: bindings are for the dev issuer" "[\"$pb_dev\"]" "$(echo "$st" | jq -c '[.identities[].issuer] | unique')"
pb_calls CreateInvite
expect_eq "dex-prebind: CreateInvite uses the admin key"  "2 admin" "$pb_n $(grep '^CreateInvite ' "$pb_dir/log" | sort -u | cut -d' ' -f2)"
pb_calls ClaimInvite
expect_eq "dex-prebind: ClaimInvite uses the bearer token" "2 bearer" "$pb_n $(grep '^ClaimInvite ' "$pb_dir/log" | sort -u | cut -d' ' -f2)"
expect_eq "dex-prebind: one token per account" "admin@example.com user@example.com" "$(sort "$pb_dir/tokens" | tr '\n' ' ' | sed 's/ $//')"
first_out=$out

before=$(wc -l < "$pb_dir/log" | tr -d ' ') ; st_before=$(pb_state); tokens_before=$(wc -l < "$pb_dir/tokens" | tr -d ' ')
pb_run
expect_eq "dex-prebind: rerun succeeds" "0" "$rc"
expect_eq "dex-prebind: rerun writes nothing (state identical)" "$st_before" "$(pb_state)"
rerun_log=$(tail -n +$((before + 1)) "$pb_dir/log")
n_mut=$(grep -cE '^(CreateAccount|CreateInvite|ClaimInvite) ' <<<"$rerun_log" || [ $? -eq 1 ])
expect_eq "dex-prebind: rerun calls no mutation" "0" "$n_mut"
expect_eq "dex-prebind: rerun asks Dex for no token" "$tokens_before" "$(wc -l < "$pb_dir/tokens" | tr -d ' ')"
expect_match "dex-prebind: rerun says already bound" 'already bound' "$out"
rerun_out=$out

# a wrong key is refused by the server: non-zero, and the key is not echoed
pb_run ADMIN_API_KEY=xk_wrong-key-secret
bad_key_rc=$rc bad_key_out=$out
if [ "$bad_key_rc" != 0 ]; then ok; else bad "dex-prebind: a rejected admin key must fail"; fi

# an issuer that is not the dev Dex is refused before anything is sent
before=$(wc -l < "$pb_dir/log" | tr -d ' ')
pb_run ISSUER=https://accounts.example.com
if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: a non-dev issuer must be refused"; fi
expect_match "dex-prebind: the refusal names the issuer rule" 'issuer' "$out"
expect_eq "dex-prebind: a non-dev issuer sends no request" "$before" "$(wc -l < "$pb_dir/log" | tr -d ' ')"
expect_eq "dex-prebind: a non-dev issuer asks Dex for no token" "$tokens_before" "$(wc -l < "$pb_dir/tokens" | tr -d ' ')"
pb_run ISSUER="${pb_dev}/"
if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: the issuer must match exactly (trailing slash)"; fi
# the admin key must not be sent to a non-local server
pb_run BASE_URL=https://rpc.example.com
if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: a non-local BASE_URL must be refused"; fi
expect_match "dex-prebind: the refusal names BASE_URL" 'BASE_URL' "$out"
pb_run ADMIN_API_KEY=
if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: a missing ADMIN_API_KEY must fail"; fi
expect_match "dex-prebind: the refusal names ADMIN_API_KEY" 'ADMIN_API_KEY' "$out"
expect_eq "dex-prebind: refusals sent nothing" "$before" "$(wc -l < "$pb_dir/log" | tr -d ' ')"

# BASE_URL is parsed, not prefix-matched: userinfo and look-alike hosts must not carry the key to a
# server the guard did not name. The stub on 127.0.0.1 stands in for the "remote" one.
before=$(wc -l < "$pb_dir/log" | tr -d ' ')
pb_port=${pb_url##*:}
for u in "http://localhost:unused@127.0.0.1:$pb_port" "http://user@127.0.0.1:$pb_port" "http://127.0.0.1@127.0.0.1:$pb_port" \
  "http://127.0.0.1:$pb_port@127.0.0.1:$pb_port" "http://127.0.0.1:$pb_port?@x" "http://127.0.0.1:$pb_port#@x" \
  "http://localhost.example.com" "http://127.0.0.1.example.com:$pb_port" "http://localhost:0" "http://localhost:65536" "http://localhost:99999" \
  "http://localhost:" "http://LOCALHOST:$pb_port" "http://127.0.0.1:$pb_port/x" "http://127.0.0.1:$pb_port\\@evil" "http://[::1]x" " http://127.0.0.1:$pb_port"; do
  pb_run BASE_URL="$u"
  if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: BASE_URL [$u] must be refused"; fi
done
# the same attack through a real name: a curl wrapper maps remote.invalid to the stub (curl's own --resolve),
# so a guard that let the userinfo URL through would really connect to the "remote" host and send the key
pb_curlbin="$tmp/pb-curlbin"; mkdir -p "$pb_curlbin"
printf '#!/usr/bin/env bash\nexec "%s" --resolve "remote.invalid:%s:127.0.0.1" "$@"\n' "$(command -v curl)" "$pb_port" > "$pb_curlbin/curl"
chmod +x "$pb_curlbin/curl"
for u in "http://localhost:unused@remote.invalid:$pb_port" "http://remote.invalid:$pb_port" "http://localhost:unused@remote.invalid:$pb_port/"; do
  pb_run PATH="$pb_curlbin:$PATH" BASE_URL="$u"
  if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: remote.invalid BASE_URL [$u] must be refused"; fi
done
expect_eq "dex-prebind: refused BASE_URLs sent nothing" "$before" "$(wc -l < "$pb_dir/log" | tr -d ' ')"
pb_run BASE_URL="$pb_url/"
expect_eq "dex-prebind: a plain local BASE_URL (trailing slash) still works" "0" "$rc"

# a server that echoes the key (or a token, or a long text) as its error code must not get it printed
refl_out=""
long80=$(printf 'a%.0s' {1..80})
for r in "$pb_key" xk_wrong-key-secret tok-secret-admin@example.com "$long80" 'A_B'; do
  pb_up refl STUB_REFLECT="$r"
  pb_run
  if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: an error answer must fail the run"; fi
  refl_out="$refl_out
$out"
  expect_match "dex-prebind: a reflected error code is redacted" '<redacted>' "$out"
  if printf '%s' "$out" | grep -qF -- "$r"; then bad "dex-prebind: output echoes the server's [$r]"; else ok; fi
done
# a code that is harmless on its own but is the key itself
pb_up refl2 STUB_REFLECT=lowerkey123
pb_run ADMIN_API_KEY=lowerkey123
if printf '%s' "$out" | grep -qF -- lowerkey123; then bad "dex-prebind: output echoes a whitelist-shaped key"; else ok; fi
# the credential the server really received, echoed back as code and/or x-error-code, in a shape the
# code whitelist would accept: the raw bearer token (not "Bearer <token>") and the raw API key
for where in code header both; do
  pb_up reflbearer STUB_REFLECT_AUTH=$where STUB_REFLECT_ON=ClaimInvite
  pb_run PB_TOKEN_PLAIN=1
  if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: a reflected bearer ($where) must fail the run"; fi
  if printf '%s' "$out" | grep -qF -- tokplain; then bad "dex-prebind: output echoes the reflected bearer token ($where)"; else ok; fi
  refl_out="$refl_out
$out"
  pb_up reflkey STUB_ADMIN_KEY=lowerkey123 STUB_REFLECT_AUTH=$where STUB_REFLECT_ON=ListAccounts
  pb_run ADMIN_API_KEY=lowerkey123
  if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: a reflected API key ($where) must fail the run"; fi
  if printf '%s' "$out" | grep -qF -- lowerkey123; then bad "dex-prebind: output echoes the reflected API key ($where)"; else ok; fi
  refl_out="$refl_out
$out"
done
# an ordinary error keeps its (whitelisted) code
pb_up plain
pb_run ADMIN_API_KEY=xk_wrong-key-secret
expect_match "dex-prebind: an ordinary error still names its codes" 'code unauthenticated, x-error-code auth.invalid_credential' "$out"

# a failed claim leaves the account and the pending invite; the rerun claims it without a second invite
pb_up partial STUB_FAIL_CLAIM=1
pb_run
if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: a failed ClaimInvite must fail the run"; fi
fail_out=$out
cp "$pb_dir/port" "$pb_dir/port.failing"
kill "${PB_PIDS##* }" 2>/dev/null || true
pb_dir2=$pb_dir
pb_up partial2
# the new stub starts empty: replay the half-done state (accounts + pending invites, no bindings) through the RPC itself
for e in admin@example.com user@example.com; do
  role='[]'; [ "$e" = admin@example.com ] && role='["admin"]'
  aid=$(curl -fsS -H "X-API-Key: $pb_key" -H 'Content-Type: application/json' -d "{\"name\":\"$e\",\"roles\":$role}" "$pb_url/identity.v1.IdentityService/CreateAccount" | jq -r .account.id)
  curl -fsS -H "X-API-Key: $pb_key" -H 'Content-Type: application/json' -d "{\"accountId\":\"$aid\",\"email\":\"$e\"}" "$pb_url/identity.v1.IdentityService/CreateInvite" >/dev/null
done
: > "$pb_dir/log"
pb_run
expect_eq "dex-prebind: resume after a failed claim succeeds" "0" "$rc"
pb_calls CreateAccount; n_acc=$pb_n
pb_calls CreateInvite
expect_eq "dex-prebind: resume creates no second account or invite" "0 0" "$n_acc $pb_n"
expect_eq "dex-prebind: resume binds both" "2" "$(pb_state | jq '.identities | length')"

# an existing account is reused only if it is what the Dex account should be: a wrong role set or a
# disabled account fails closed (non-zero, names the email), changes nothing, grants nothing
pb_call() { curl -fsS -H "X-API-Key: $pb_key" -H 'Content-Type: application/json' -d "$2" "$pb_url/identity.v1.IdentityService/$1"; }
for case in "admin@example.com|[]|admin role missing" "user@example.com|[\"admin\"]|user holds a role" "admin@example.com|[\"admin\",\"ops\"]|admin has an extra role"; do
  IFS='|' read -r who roles why <<<"$case"
  pb_up mismatch
  pb_call CreateAccount "{\"name\":\"$who\",\"roles\":$roles}" >/dev/null
  pb_run
  if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: existing account mismatch ($why) must fail"; fi
  expect_match "dex-prebind: the mismatch names the email ($why)" "$who" "$out"
  expect_eq "dex-prebind: a mismatch binds and invites nothing ($why)" "0" "$(pb_state | jq '(.invites|length) + (.identities|length)')"
  expect_eq "dex-prebind: a mismatch asks Dex for no token ($why)" "0" "$(wc -l < "$pb_dir/tokens" | tr -d ' ')"
done
pb_up disabled
pb_run
acct=$(pb_state | jq -r '.accounts[] | select(.name=="user@example.com") | .id')
pb_call DisableAccount "{\"id\":\"$acct\"}" >/dev/null
pb_run
if [ "$rc" != 0 ]; then ok; else bad "dex-prebind: a disabled account must fail the run, even when bound"; fi
expect_match "dex-prebind: the disabled account is named" 'user@example.com' "$out"

# no stub outlives the suite: its processes are in PB_PIDS and the EXIT trap (the later prepull trap
# must not replace the one that kills them)
expect_match "test.sh's EXIT trap kills the dex-prebind stubs" 'PB_PIDS' "$(trap -p EXIT)"

# nothing secret in any output: not the admin key (right or wrong), not a token
all_out="$first_out
$rerun_out
$bad_key_out
$fail_out
$refl_out
$out"
for secret in "$pb_key" xk_wrong-key-secret tok-secret- tokplain lowerkey123; do
  if printf '%s' "$all_out" | grep -qF -- "$secret"; then bad "dex-prebind: output leaks [$secret]"; else ok; fi
done

# tests/dex-prebind/ is a scripts-test input outside scripts/ (ci-changes.sh scripts_src)
expect_eq "dex-prebind stub also runs scripts-test" "scripts_test" "$(jobs_on dexstub tests/dex-prebind/stub_rpc.py)"

# ── web/apps/console/deploy/18-console-env.envsh: the console image's OIDC env gate (#273) ──
# The same URL judgement as the SPA's parseConfig (new URL): what the container accepts the browser
# must accept too, otherwise OIDC is switched off silently on the client. Run under sh and dash.
envsh="$src/web/apps/console/deploy/18-console-env.envsh"
envsh_rc() { # <issuer> <client id> → exit code of sourcing the gate
  local rc=0
  CONSOLE_OIDC_ISSUER=$1 CONSOLE_OIDC_CLIENT_ID=$2 sh -c ". \"$envsh\"" >/dev/null 2>&1 || rc=$?
  echo "$rc"
}
for good in https://idp.example.com https://idp.example.com/dex http://127.0.0.1:18121/idp http://dex.localtest.me:5556/dex https://idp.example.com:65535/a/b https://10.0.0.255:1/x; do
  expect_eq "envsh accepts $good" 0 "$(envsh_rc "$good" x-console)"
done
for badv in 'https://idp.example.com:65536/dex' 'https://idp.example.com:99999' 'https://idp.example.com:0/dex' 'https://10.0.0.256/dex' 'https://idp.example.com.123/dex' 'https://idp.example.com.0x1' 'https://idp.example.com.0XFF/dex' 'https://0x7f.0x1' 'https://idp.example.com.0x' 'https://idp..example.com' 'https://-idp.example.com' 'https://idp.example.com/dex?x=1' 'https://u@idp.example.com' 'ftp://idp.example.com' 'https://idp.example.com/de x'; do
  expect_eq "envsh refuses $badv" 1 "$(envsh_rc "$badv" x-console)"
done
expect_eq "envsh refuses an issuer with a newline"          1 "$(envsh_rc "$(printf 'https://idp.example.com/dex\njunk')" x-console)"
expect_eq "envsh refuses an issuer with an inner newline+."  1 "$(envsh_rc "$(printf 'https://idp.example.com/dex\n.')" x-console)"
expect_eq "envsh refuses a client id with a newline"        1 "$(envsh_rc https://idp.example.com "$(printf 'a\nb')")"
expect_eq "envsh refuses an issuer without a client id"     1 "$(envsh_rc https://idp.example.com '')"
expect_eq "envsh: nothing set is OIDC off, not an error"    0 "$(envsh_rc '' '')"
# Whatever the gate accepts, the browser's URL parser accepts with the same origin (needs node).
if command -v node >/dev/null 2>&1; then
  for cand in https://idp.example.com.0x1 https://idp.example.com.0XFF/dex https://idp.example.com:65536/dex https://10.0.0.256/dex https://idp.example.com.123/dex https://idp..example.com https://idp.example.com:0/dex https://idp.example.com:65535/dex http://127.0.0.1:18121/idp; do
    gate=$(envsh_rc "$cand" x-console)
    url=$(node -e 'try { const u = new URL(process.argv[1]); console.log(u.origin === "null" ? "bad" : "ok") } catch { console.log("bad") }' "$cand")
    if [ "$gate" = 0 ] && [ "$url" != ok ]; then bad "envsh accepts $cand but the SPA's URL parser rejects it (OIDC would be silently off)"; else ok; fi
  done
fi

# ── scripts/suite-guard.sh ─────────────────────────────────────────────────────────
# The outer gate of this suite (#307): test.sh re-executes itself under suite-guard.sh, which runs it
# in a process group of its own and, when it ends — normally, failed, or interrupted — fails if
# anything is still alive in that group, then kills (TERM, then KILL) and waits for it. Exercised here
# with fake suites that leave a background process behind; the pid of the leftover is in $PIDF.
guard="$src/scripts/suite-guard.sh"
# this very run is inside the gate (test.sh re-executed itself through it)
expect_eq "guard: this suite runs under suite-guard.sh" "1" "${SUITE_GUARD:-0}"
gfake="$tmp/guard-fake.sh"
cat > "$gfake" <<'FAKE'
#!/usr/bin/env bash
# $1 = clean | leak | leak-fail | exit3 | stubborn | hang | deaf
case $1 in
  leak|leak-fail|hang) sleep 300 & echo $! > "$PIDF" ;;
  stubborn) ( trap '' TERM HUP INT; exec sleep 300 ) & echo $! > "$PIDF" ;;
  deaf) trap '' TERM INT HUP; sleep 300 & echo $! > "$PIDF"; echo $$ > "$PIDF.leader"; while :; do sleep 1; done ;;   # the suite itself ignores TERM / INT and keeps going
esac
case $1 in
  leak-fail) exit 1 ;;
  exit3) exit 3 ;;
  hang) sleep 300 ;;
esac
exit 0
FAKE
chmod +x "$gfake"
# gone <pid-file>: true when the recorded pid no longer exists
gone() { ! kill -0 "$(cat "$1")" 2>/dev/null; }
# guard_run <mode>: runs the guard over the fake; sets out, rc and gpid (file) for the leftover.
guard_run() {
  gpid="$tmp/guard-pid-$1"; rm -f "$gpid"; rc=0
  out=$(PIDF="$gpid" "$guard" "$gfake" "$1" 2>&1) || rc=$?
}
# a bystander outside the suite's group must survive every case below
sleep 300 & bystander=$!
guard_run clean
expect_eq "guard: a clean suite passes"                            "0" "$rc"
expect_eq "guard: a suite's own exit code is kept"                 "3" "$(guard_run exit3; echo $rc)"
guard_run leak
if [ "$rc" != 0 ]; then ok; else bad "guard: a suite that leaves a process behind must fail"; fi
expect_match "guard: the leftover is reported"                     'left .* running' "$out"
if [ -s "$gpid" ] && gone "$gpid"; then ok; else bad "guard: the leftover must be killed and waited for"; fi
guard_run leak-fail
expect_eq "guard: a failed suite with a leftover keeps its failure" "1" "$rc"
expect_match "guard: a failed suite's leftover is reported too"    'left .* running' "$out"
if [ -s "$gpid" ] && gone "$gpid"; then ok; else bad "guard: a failed suite's leftover must be killed"; fi
guard_run stubborn
if [ "$rc" != 0 ] && [ -s "$gpid" ] && gone "$gpid"; then ok; else bad "guard: a leftover that ignores TERM must still be killed (rc=$rc)"; fi
# interrupted: INT / TERM to the guard stop the suite and everything it started
for sig in INT TERM; do
  gpid="$tmp/guard-pid-hang-$sig"; rm -f "$gpid"
  # python restores the default SIGINT: a non-interactive shell started as a background job (a CI
  # runner, this suite run from another script) has SIGINT ignored, which nothing downstream can trap
  PIDF="$gpid" python3 -c 'import os, signal, sys; signal.signal(signal.SIGINT, signal.SIG_DFL); os.execv(sys.argv[1], sys.argv[1:])' "$guard" "$gfake" hang >/dev/null 2>&1 &
  g=$!
  i=0; while [ ! -s "$gpid" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
  kill -$sig "$g" 2>/dev/null || true
  rc=0; wait "$g" || rc=$?
  if [ "$sig" = INT ]; then want=130; else want=143; fi
  expect_eq "guard: $sig stops the guard with $want" "$want" "$rc"
  if [ -s "$gpid" ] && gone "$gpid"; then ok; else bad "guard: $sig must take the suite's leftover with it"; fi
done
# the suite (the group's leader) ignoring TERM / INT must not stop the guard from finishing: after a
# short grace the guard escalates to KILL for the whole group (SUITE_GUARD_GRACE seconds, 1 here)
for sig in INT TERM; do
  gpid="$tmp/guard-pid-deaf-$sig"; rm -f "$gpid" "$gpid.leader"
  SUITE_GUARD_GRACE=1 PIDF="$gpid" python3 -c 'import os, signal, sys; signal.signal(signal.SIGINT, signal.SIG_DFL); os.execv(sys.argv[1], sys.argv[1:])' "$guard" "$gfake" deaf >/dev/null 2>&1 &
  g=$!
  i=0; while [ ! -s "$gpid.leader" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
  kill -$sig "$g" 2>/dev/null || true
  i=0; while kill -0 "$g" 2>/dev/null && [ $i -lt 100 ]; do sleep 0.1; i=$((i + 1)); done   # at most 10 s
  if kill -0 "$g" 2>/dev/null; then bad "guard: $sig to a suite that ignores it must still end within the grace (guard still running after 10 s)"; kill -9 "$g" 2>/dev/null || true; else ok; fi
  rc=0; wait "$g" 2>/dev/null || rc=$?
  if [ "$sig" = INT ]; then want=130; else want=143; fi
  expect_eq "guard: $sig to a deaf suite exits $want" "$want" "$rc"
  if [ -s "$gpid" ] && gone "$gpid" && gone "$gpid.leader"; then ok; else bad "guard: $sig must take a deaf suite and its child down"; fi
  for f in "$gpid" "$gpid.leader"; do if [ -s "$f" ]; then kill -9 "$(cat "$f")" 2>/dev/null || true; fi; done
done
if kill -0 "$bystander" 2>/dev/null; then ok; else bad "guard: a process outside the suite's group was killed"; fi
kill "$bystander" 2>/dev/null || true
for f in "$tmp"/guard-pid-*; do if [ -s "$f" ]; then kill -9 "$(cat "$f")" 2>/dev/null || true; fi; done

# ── release image lists ────────────────────────────────────────────────────────────────────────
# The images a release ships are named in five places, and adding an app to one and forgetting another
# gives a release whose images are not all built, signed, verified or documented (payment-rpc, #367
# slice 5a): .goreleaser.yaml (dockers_v2: the Go images), release.yml (the summary of what was pushed),
# scripts/release-verify.sh (the signature check of each), release-drafter.yml (the Images block of the
# notes) and Taskfile.yml's APPS (every deployable app). The console is the one image with no Go binary:
# it is in the last three, built by release.yml with buildx. An image added to any list alone is a red
# scripts-test, not a release found wrong afterwards.
words() { tr ' ' '\n' | sed '/^$/d' | sort | tr '\n' ' ' | sed 's/ $//'; }
images_in() { grep -o 'ghcr\.io/blackhorseya/x-monorepo/[a-z-]*' "$1" | sed 's#.*/##' | words; }
go_images=$(images_in "$src/.goreleaser.yaml")
apps=$(sed -n 's/^  APPS: //p' "$src/Taskfile.yml" | words)
with_console=$(printf '%s console' "$go_images" | words)
expect_match "release images: .goreleaser.yaml lists images (positive control)" "rpc" "$go_images"
expect_eq "release images: every app in Taskfile APPS ships a goreleaser image, and nothing else does" "$apps" "$go_images"
expect_eq "release images: release-verify.sh checks each image's signature" "$with_console" \
  "$(sed -n 's/^for app in \(.*\); do$/\1/p' "$src/scripts/release-verify.sh" | words)"
expect_eq "release images: release.yml's summary names each pushed image" "$with_console" \
  "$(sed -n 's/^ *for app in \(.*\); do$/\1/p' "$src/.github/workflows/release.yml" | words)"
expect_eq "release images: the release notes' Images block lists each" "$with_console" \
  "$(grep -o '^  ghcr\.io/blackhorseya/x-monorepo/[a-z-]*:\$RESOLVED_VERSION' "$src/.github/release-drafter.yml" | sed 's#.*/##; s#:.*##' | words)"
expect_match "release images: ci.yml's release-snapshot job smokes the payment-rpc image" "scripts/image-smoke.sh payment-rpc" "$(cat "$src/.github/workflows/ci.yml")"

# payment-worker (#410): the module is in go.work, so a change under it selects its tests, and moving consume.go from
# payment-rpc to payment-worker (a delete plus an add: --no-renames) still selects payment-rpc's tests as well.
aff() { (cd "$src" && scripts/affected.sh --files "$@" | tr '\n' ' ' | sed 's/ $//'); }
expect_eq "affected: apps/payment-worker is a module of go.work" "apps/payment-worker" "$(aff apps/payment-worker/main.go)"
expect_eq "affected: consume.go moved from payment-rpc to payment-worker selects both" "apps/payment-rpc apps/payment-worker" \
  "$(aff apps/payment-rpc/consume.go apps/payment-worker/consume.go)"
expect_match "release images: ci.yml's release-snapshot job smokes the payment-worker image" "scripts/image-smoke.sh payment-worker" "$(cat "$src/.github/workflows/ci.yml")"

# ── MODULES is derived from go.work; check-modules.sh refuses go.work paths it cannot carry (#413) ──────────────
# lint / test / cover / tidy loop over Taskfile.yml's MODULES. It used to be a second hand-kept list, and a module only
# in go.work was silently never linted or tested (apps/payment-worker, #412); five rounds of a gate comparing the two
# each hit a path the text channel between them could not carry. MODULES is now `go work edit -json | jq` of go.work,
# so a module added there is iterated by every per-module task, and scripts/check-modules.sh (task check-work-sync,
# CI work-sync) only refuses the go.work shapes the derivation cannot carry. Skipped without go / jq / task on PATH like
# the other optional cases — but never in CI ($CI is set on GitHub Actions), where a missing tool must be a red run, not
# a skipped gate (the scripts-test job runs ./.github/actions/setup for go + task; jq is on the ubuntu runner image).
if [ -n "${CI:-}" ] || { command -v go >/dev/null 2>&1 && command -v task >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; }; then
cm="$src/scripts/check-modules.sh"
# gwrun <name> <go.work body>: runs the check on a throwaway go.work; sets $rc and $out.
gwrun() {
  local d="$tmp/modules-$1"
  mkdir -p "$d"
  printf '%s\n' "$2" > "$d/go.work"
  rc=0; out=$("$cm" "$d/go.work" 2>&1) || rc=$?
}
gw_block='go 1.27

use (
	./apps/rpc
	./pkgs/kernel
	./tools/x
)'
gwrun ok "$gw_block"
expect_eq "check-modules: a go.work of ordinary paths passes" "0" "$rc"
expect_match "check-modules: a pass names how many modules it saw (non-empty positive control)" "3 modules" "$out"
gwrun single 'go 1.27

use ./apps/rpc
use ./pkgs/kernel'
expect_eq "check-modules: single-line use directives are read" "0" "$rc"
expect_match "check-modules: single-line use directives are counted" "2 modules" "$out"
gwrun empty "go 1.27"
expect_eq "check-modules: an empty go.work fails" "1" "$rc"
expect_match "check-modules: the empty go.work says so" "go.work lists no modules" "$out"
# A go.work path outside the allowlist [A-Za-z0-9._/-] (not starting with "-") is refused, shown JSON-quoted: the per-module
# loops splice it into shell commands (#413 r3 newline / whitespace, r6 `$UNSET`). refused <name> <path as written inside
# go.work's quotes> <the path's JSON form, as a regex>.
refused() {
  gwrun "$1" "go 1.27

use \"./apps/ok\"
use \"$2\""
  expect_eq "check-modules: a go.work path with $1 is refused" "1" "$rc"
  expect_match "check-modules: the refusal for $1 says MODULES cannot carry it" 'has characters outside \[A-Za-z0-9\._/-\].*MODULES cannot carry it' "$out"
  expect_match "check-modules: the refused $1 path is shown JSON-quoted" "go\\.work module path \"\\./apps/$3\" has" "$out"
}
refused newline './apps/a\nb' 'a\\nb'
refused space './apps/a b' 'a b'
refused tab './apps/a\tb' 'a\\tb'
refused nbsp $'./apps/a\xc2\xa0b' $'a\xc2\xa0b'
refused dollar './apps/a$UNSET' 'a\$UNSET'
refused squote "./apps/a'b" "a'b"
refused backtick './apps/a`b' 'a`b'
refused semicolon './apps/a;b' 'a;b'
refused ampersand './apps/a&b' 'a&b'
refused star './apps/a*' 'a\*'
gwrun leading-dash 'go 1.27

use "./apps/ok"
use "./-x"'
expect_eq "check-modules: a go.work path starting with - (an option to cd) is refused" "1" "$rc"
expect_match "check-modules: the refused leading-dash path is shown JSON-quoted" 'go\.work module path "\./-x" has' "$out"
gwrun plain 'go 1.27

use (
	./apps/a.b
	./pkgs/x_y-z
	./tools/Z9
)'
expect_eq "check-modules: dots, underscores, dashes and digits inside a path are accepted" "0" "$rc"
rc=0; out=$("$cm" "$tmp/no-such-go.work" 2>&1) || rc=$?
expect_eq "check-modules: an unreadable go.work fails" "1" "$rc"
rc=0; out=$(cd "$src" && "$cm" 2>&1) || rc=$?
expect_eq "check-modules: this repo's go.work passes" "0" "$rc"
expect_match "check-modules: this repo's check saw a non-empty set" "[1-9][0-9]* modules" "$out"
# A missing tool is a failure naming it, never a skip: run with a PATH that has everything but one tool.
tools_without() {
  local bindir="$tmp/toolbin-$1" t
  mkdir -p "$bindir"
  for t in bash dirname go jq; do
    if [ "$t" != "$1" ] && [ ! -e "$bindir/$t" ] && tp=$(command -v "$t"); then ln -s "$tp" "$bindir/$t"; fi   # a tool the host lacks is just not linked into the stripped PATH; the case that needs it fails on its own
  done
  echo "$bindir"
}
for missing in go jq; do
  d="$tmp/modules-no-$missing"; mkdir -p "$d"
  printf '%s\n' "$gw_block" > "$d/go.work"
  rc=0; out=$(PATH=$(tools_without "$missing") "$(command -v bash)" "$cm" "$d/go.work" 2>&1) || rc=$?
  expect_eq "check-modules: without $missing the check fails" "1" "$rc"
  expect_match "check-modules: without $missing the failure names it" "$missing not found" "$out"
done

# MODULES is derived from go.work: the real Taskfile.yml and a go.work with one more `use`, run through `task --dry`
# (dry still resolves vars). A module added to go.work only is iterated by every per-module task.
d="$tmp/modules-derived"; mkdir -p "$d"
cp "$src/Taskfile.yml" "$d/Taskfile.yml"
cp -R "$src/scripts" "$d/scripts"
cp "$src/go.work" "$d/go.work"
printf '\nuse ./apps/brand-new\n' >> "$d/go.work"
for t in lint test tidy; do
  rc=0; out=$(cd "$d" && task --dry "$t" 2>&1) || rc=$?
  expect_match "derived modules: task $t runs for an existing module (positive control)" 'cd pkgs/kernel' "$out"
  expect_match "derived modules: task $t iterates a module only go.work lists" 'cd apps/brand-new' "$out"
done
# The reviewer's r6 case from the real entry: a module directory named `apps/a$UNSET` beside `apps/a`. With the path spliced
# unquoted into `cd`, the shell expanded $UNSET (empty) and `task test` entered apps/a twice, passed, and never tested the
# other module. Two real Go modules, one test each; `task test` must fail, not pass having run only the first.
d="$tmp/modules-dollar"; mkdir -p "$d/apps/a" "$d/apps/a\$UNSET"
cp "$src/Taskfile.yml" "$d/Taskfile.yml"; cp -R "$src/scripts" "$d/scripts"
printf 'module example.com/base\n\ngo 1.27\n' > "$d/apps/a/go.mod"
printf 'package a\n\nimport "testing"\n\nfunc TestBase(t *testing.T) {}\n' > "$d/apps/a/a_test.go"
printf 'module example.com/target\n\ngo 1.27\n' > "$d/apps/a\$UNSET/go.mod"
printf 'package a\n\nimport "testing"\n\nfunc TestTarget(t *testing.T) {}\n' > "$d/apps/a\$UNSET/a_test.go"
printf 'go 1.27\n\nuse (\n\t./apps/a\n\t"./apps/a$UNSET"\n)\n' > "$d/go.work"
rc=0; out=$(cd "$d" && UNSET= task test 2>&1) || rc=$?
if [ "$rc" = 0 ] && ! printf '%s' "$out" | grep -q 'example.com/target'; then bad "derived modules: task test passed without testing apps/a\$UNSET (it ran apps/a twice)"; else ok; fi
expect_eq "derived modules: task test fails on a module path it cannot carry" "1" "$rc"
expect_match "derived modules: the failure names the path" 'apps/a\$UNSET' "$out"
# A go that cannot read go.work must fail the task, not leave MODULES empty (every per-module loop would iterate nothing).
mkdir -p "$tmp/failing-go"; printf '#!/bin/sh\necho "go: stub failure" >&2\nexit 1\n' > "$tmp/failing-go/go"; chmod +x "$tmp/failing-go/go"
rc=0; out=$(cd "$d" && PATH="$tmp/failing-go:$PATH" task --dry lint 2>&1) || rc=$?
expect_eq "derived modules: a failing go work edit fails the task instead of an empty MODULES" "1" "$rc"
expect_match "derived modules: the failing task shows go's error" "go: stub failure" "$out"
fi

# ── check-script-modes.sh: `task tools` chmods must not change the index (#454) ─────────────────────────────────
# The repo itself: every file the chmod line names is 100755 in the index. A miss in the tools cache runs that chmod in
# gen-freshness; a 100644 file then shows up as a mode-only diff and fails the job (lock.sh and runner-home-test.sh did).
rc=0; out=$(cd "$src" && scripts/check-script-modes.sh 2>&1) || rc=$?
expect_eq "script modes: this repo's chmod list is all 100755 in the index" "0" "$rc"
expect_match "script modes: the anchor — it checked the kind scripts" 'files, all 100755' "$out"
# Fixture: a minimal repo whose Taskfile has the chmod line and 20 scripts. Baseline green; each mutant must go red.
modes_fixture() {
  repo="$tmp/modes-$1"; mkdir -p "$repo/scripts" "$repo/k"
  cp "$src/scripts/check-script-modes.sh" "$repo/scripts/"
  printf 'tasks:\n  tools:\n    cmds:\n      - chmod +x scripts/*.sh k/*.sh\n' > "$repo/Taskfile.yml"
  for ((i = 1; i <= 24; i++)); do printf '#!/bin/sh\n' > "$repo/k/s$i.sh"; chmod +x "$repo/k/s$i.sh"; done
  git -C "$repo" init -q -b main
  git -C "$repo" add -A
  git -C "$repo" commit -q -m base
}
modes_run() { rc=0; out=$(cd "$repo" && scripts/check-script-modes.sh 2>&1) || rc=$?; }
modes_fixture base; modes_run
expect_eq "script modes: fixture baseline is green" "0" "$rc"
# The gate says what `git diff` would after the chmod: apply the chmod and diff, for the baseline and the mutant.
(cd "$repo" && chmod +x scripts/*.sh k/*.sh); expect_eq "script modes: baseline chmod leaves git diff clean" "" "$(git -C "$repo" diff --stat)"
modes_fixture mut1; git -C "$repo" update-index --chmod=-x k/s7.sh; git -C "$repo" commit -q -m mutant; modes_run
expect_eq "script modes: mutant — one script back to 100644 is red" "1" "$rc"
expect_match "script modes: the mutant is named" 'k/s7\.sh is \[100644\]' "$out"
(cd "$repo" && chmod +x scripts/*.sh k/*.sh); expect_match "script modes: and the chmod really does dirty git diff" 'k/s7\.sh' "$(git -C "$repo" diff --stat)"
modes_fixture mut2; git -C "$repo" rm -q --cached k/s3.sh; modes_run
expect_eq "script modes: mutant — an untracked script is red" "1" "$rc"
modes_fixture mut3; sed -i.bak 's#k/\*.sh#kk/*.sh#' "$repo/Taskfile.yml"; rm "$repo/Taskfile.yml.bak"; modes_run
expect_eq "script modes: a glob that matches nothing is not green (exit 2: too few files checked)" "2" "$rc"
expect_match "script modes: and it says which pattern matched nothing" "kk/\\*\\.sh' \\(from the chmod line\\) matches no file" "$out"
modes_fixture mut4; sed -i.bak 's#chmod +x#chmod -x#' "$repo/Taskfile.yml"; rm "$repo/Taskfile.yml.bak"; modes_run
expect_eq "script modes: no chmod line at all is not green (judge cannot pass on empty)" "2" "$rc"
# Fault injection (#454 r1, helper: scripts/fault-inject.sh, #462): each external command the gate calls prints its real output
# and THEN dies. A gate that swallows the status (`x=$(cmd || true)`, a pipe in $(…)) stays green on exactly this; it must go red.
for t in grep git dirname; do
  modes_fixture "dies-$t"
  rc=0; out=$(cd "$repo" && PATH="$(inject_fail "$tmp/inj-modes-$t" "$t" 42):$PATH" scripts/check-script-modes.sh 2>&1) || rc=$?
  if [ "$rc" = 0 ]; then bad "script modes: $t prints its output then exits 42, the gate still exits 0 [$out]"; else ok; fi
done
# ... and the same with the output cut short (nothing, or one line): a half-read answer is not an answer either.
for t in grep git; do for keep in 0 1; do
  modes_fixture "dies-$t-$keep"
  rc=0; out=$(cd "$repo" && PATH="$(inject_fail "$tmp/inj-modes-$t-$keep" "$t" 42 "$keep"):$PATH" scripts/check-script-modes.sh 2>&1) || rc=$?
  if [ "$rc" = 0 ]; then bad "script modes: $t prints $keep line(s) then exits 42, the gate still exits 0 [$out]"; else ok; fi
done; done
# The fixture itself is green without the wrapper (so the red above is the wrapper's doing, not the fixture's).
modes_fixture dies-control; modes_run
expect_eq "script modes: fault-injection control — same fixture, real tools, is green" "0" "$rc"

# ── check-exit-swallow.sh (#462) ────────────────────────────────────────────────────────────────
# The static gate: a script may not drop an external command's exit status without saying why. One mutant per shape it
# must catch (each a file of its own, red and naming its rule), the exemption flipping each to green, the lines it must
# NOT flag, and the checker's own external commands dying (exit 2, never "clean").
sw="$tmp/swallow"; mkdir -p "$sw"
# sw_file <name> <body…>: $sw/<name>.sh with pipefail set (so only the shape under test can be flagged)
sw_file() { local n=$1; shift; printf '#!/usr/bin/env bash\nset -euo pipefail\n%s\n' "$*" > "$sw/$n.sh"; }
sw_run() { rc=0; out=$("$src/scripts/check-exit-swallow.sh" "$@" 2>&1) || rc=$?; }
# sw_red <name> <rule> <body…>: the body is red, the output names the rule, and an exit-ok comment makes it green
sw_red() {
  local n=$1 rule=$2; shift 2
  sw_file "$n" "$*"; sw_run "$sw/$n.sh"
  expect_eq    "swallow: $n is red" "1" "$rc"
  expect_match "swallow: $n names the rule $rule" ": $rule:" "$out"
  sw_file "$n-ok" "$* # exit-ok: the verdict is read from the file this writes, not from this status"; sw_run "$sw/$n-ok.sh"
  expect_eq    "swallow: $n with an exit-ok comment is green" "0" "$rc"
}
# the table: name @@ rule @@ the line that must be red
while IFS= read -r row; do
  [ -n "$row" ] || continue
  n=${row%% @@ *}; rest=${row#* @@ }; rule=${rest%% @@ *}; body=${rest#* @@ }
  sw_red "$n" "$rule" "$body"
done <<'ROWS'
cmdsub @@ swallow @@ x=$(grep a f || true)
direct @@ swallow @@ grep a f > o || true
catfile @@ swallow @@ cat x || true
colon @@ swallow @@ grep a f || :
inpipe @@ swallow @@ n=$(grep a f | wc -l || true)
inquote @@ swallow @@ x="$(grep a f || true)"
intrap @@ swallow @@ trap 'cleanup || true' EXIT
retzero @@ swallow @@ mktemp -d || return 0
forseq @@ forlist @@ for _ in $(seq 1 5); do :; done
forpgrep @@ forlist @@ for p in $(pgrep -P 1); do :; done
setargs @@ setargs @@ set -- $(gh pr view)
waitalone @@ swallow @@ wait "$p" || true
argeq @@ argsub @@ expect_eq x 1 "$(cat f | grep -c y)"
argtest @@ argsub @@ [ "$(grep -c y f)" -ge 2 ] || exit 1
argcount @@ argsub @@ expect_eq x 1 "$(cat f | count_lines y)"
localcount @@ declsub @@ f() { local n="$(grep -c y f)"; echo "$n"; }
localplain @@ declsub @@ local out=$(curl -fsS "$u")
declarer @@ declsub @@ declare -r n=$(cmd)
exportsub @@ declsub @@ export X="$(cmd)"
readonlysub @@ declsub @@ readonly n=$(cmd)
localmulti @@ declsub @@ local a=1 b="$(cmd)"
localspace @@ declsub @@ local a="hello world" b="$(grep -c y f)"
localsq @@ declsub @@ local a='hello world' b="$(cmd)"
exportspace @@ declsub @@ export A="x y" B=$(cmd)
mixedquote @@ declsub @@ local a="x"'y z' b=$(cmd)
ansic @@ declsub @@ local a=$'q r' b=$(cmd)
innersub @@ declsub @@ local a="$(inner)" b=$(cmd)
typesetsub @@ declsub @@ typeset M=$(cmd)
besub @@ besteffort @@ out=$(best_effort cat f)
bequoted @@ besteffort @@ n="$(best_effort grep -c y f)"
beassign @@ besteffort @@ x=best_effort
ROWS
# sw_write <name>: $sw/<name>.sh from stdin, after a header that sets (or not) pipefail
sw_write() { { printf '#!/usr/bin/env bash\n%s\n' "${2:-set -euo pipefail}"; cat; } > "$sw/$1.sh"; }
# no pipefail: a pipeline's status is its last command's
sw_write nopipefail 'set -eu' <<'F'
x=$(a | b)
F
sw_run "$sw/nopipefail.sh"
expect_eq    "swallow: a pipeline in a file without pipefail is red" "1" "$rc"
expect_match "swallow: and it says pipeline" ': pipeline:' "$out"
sw_write nopipefail-ok 'set -eu' <<'F'
x=$(a | b) # exit-ok: only b matters, a is a plain echo
F
sw_run "$sw/nopipefail-ok.sh"
expect_eq    "swallow: the same with an exit-ok comment is green" "0" "$rc"
sw_write pipefail <<'F'
x=$(a | b)
F
sw_run "$sw/pipefail.sh"
expect_eq    "swallow: the same pipeline with pipefail is green" "0" "$rc"
# an exemption has to carry its reason
sw_write noreason <<'F'
x=$(grep a f || true) # exit-ok:
F
sw_run "$sw/noreason.sh"
expect_eq    "swallow: an exit-ok comment without a reason does not count" "1" "$rc"
# what is not flagged
sw_write cleanup <<'F'
kill "$p" 2>/dev/null || true
rm -f x || true
"$tool" rm -f "$n" >/dev/null 2>&1 || true
[ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null || true
trap 'test -z "$pf" || kill $pf 2>/dev/null || true' EXIT
[ -n "$x" ] || return 0
kill -9 "$(cat "$f")" 2>/dev/null || true
kill "${PB_PIDS##* }" 2>/dev/null || true
F
sw_run "$sw/cleanup.sh"
expect_eq    "swallow: kill / rm cleanup and [ ] guards are not flagged" "0" "$rc"
sw_write canonical <<'F'
n=$(grep -c a f || [ $? -eq 1 ])
for ((i = 1; i <= 5; i++)); do :; done
rc=0; out=$(cmd) || rc=$?
for p in $pids; do :; done
for f in "$d"/log-*.txt; do n=$(wc -l < "$f"); done
for a in 'NS=a$(id)' 'x'; do :; done
n=$(grep -c y f || [ $? -eq 1 ])
n=$(cat f | count_lines y)
expect_eq x 1 "$n" "$(date)"
f() { local n; n=$(grep -c y f || [ $? -eq 1 ]); local m=$((n + 1)); local k=1; echo "$n $m $k"; }
best_effort kubectl -n "$ns" get pods,jobs >&2
[ -n "$x" ] && best_effort kubectl wait --for=delete pod -l a=b
n=$(cmd) && echo "$n"
F
sw_run "$sw/canonical.sh"
expect_eq    "swallow: the documented replacements are clean" "0" "$rc"
sw_write doc <<'F'
# x=$(a || true) is only a comment
cat <<'EOF'
x=$(a || true)
for _ in $(seq 1 3); do :; done
EOF
cat <<-X
	x=$(a || true)
	X
echo done
F
sw_run "$sw/doc.sh"
expect_eq    "swallow: comments and heredoc bodies are not scanned" "0" "$rc"
sw_write after-heredoc <<'F'
cat <<EOF
x
EOF
x=$(grep a f || true)
F
sw_run "$sw/after-heredoc.sh"
expect_eq    "swallow: a line after a heredoc is scanned again" "1" "$rc"
# declsub, the parts a line scanner cannot see: a continuation line is found by the shellcheck SC2155 layer
sw_write declcont <<'F'
f() {
  local h=1 \
    i=$(cmd)
  echo "$h $i"
}
F
sw_run "$sw/declcont.sh"
expect_eq    "swallow: a declaration split over two lines is red (shellcheck SC2155)" "1" "$rc"
expect_match "swallow: ... and says declsub" ': declsub:' "$out"
sw_write declcont-ok <<'F'
f() {
  local h=1 \
    i=$(cmd) # exit-ok: i is only logged below
  echo "$h $i"
}
F
sw_run "$sw/declcont-ok.sh"
expect_eq    "swallow: ... and an exit-ok on the line makes it green" "0" "$rc"
# a file shellcheck/bash cannot parse is an error, never a clean pass (-i SC2155 would hide the parse error)
sw_write unparsable <<'F'
if then fi (
F
sw_run "$sw/unparsable.sh"
expect_eq    "swallow: a script that does not parse is exit 2, not clean" "2" "$rc"
# shellcheck dying (after printing, or not printing) is exit 2 too
sw_file clean 'echo ok'
for keep in all 0; do
  rc=0; out=$(PATH="$(inject_fail "$tmp/inj-sw-sc-$keep" shellcheck 42 "$keep"):$PATH" "$src/scripts/check-exit-swallow.sh" "$sw/clean.sh" 2>&1) || rc=$?
  expect_eq  "swallow: shellcheck prints [$keep] then exits 42 — exit 2, not clean" "2" "$rc"
done
# scripts/best-effort.sh: a failing command is REPORTED (stderr) and the shell carries on with status 0; a succeeding one is silent.
# Whatever the command printed (all of it, part of it, nothing) before dying with 42 still reaches stdout. (seq: an external tool,
# so the PATH wrapper is what runs; a builtin like printf would not be wrapped.)
. "$src/scripts/best-effort.sh"
be_run() {   # be_run <keep> <command…>: runs best_effort on <command…> whose tool dies with 42 after <keep> lines; stdout+rc → $tmp/be.out, stderr → $tmp/be.err
  local keep=$1 w; shift
  w=$(inject_fail "$tmp/inj-be-$RANDOM" "$1" 42 "$keep" '*')
  ( PATH="$w:$PATH"; best_effort "$@"; echo "rc=$?" ) > "$tmp/be.out" 2> "$tmp/be.err"
}
for keep in all 1 0; do
  be_run "$keep" seq 1 2
  expect_match "best_effort: seq prints [$keep] then dies — it reports the exit code" 'best-effort command failed \(exit 42\): seq 1 2' "$(cat "$tmp/be.err")"
  expect_match "best_effort: ... and still returns 0" 'rc=0$' "$(cat "$tmp/be.out")"
  case $keep in
    all) expect_eq "best_effort: ... the whole output got through" "1 2 rc=0" "$(tr '\n' ' ' < "$tmp/be.out" | sed 's/ $//')" ;;
    1)   expect_eq "best_effort: ... only the first line got through" "1 rc=0" "$(tr '\n' ' ' < "$tmp/be.out" | sed 's/ $//')" ;;
    0)   expect_eq "best_effort: ... nothing got through" "rc=0" "$(cat "$tmp/be.out")" ;;
  esac
done
( best_effort seq 1 2; echo "rc=$?" ) > "$tmp/be.out" 2> "$tmp/be.err"
expect_eq    "best_effort: a succeeding command is silent on stderr" "" "$(cat "$tmp/be.err")"
expect_eq    "best_effort: ... passes its output and returns 0" "1 2 rc=0" "$(tr '\n' ' ' < "$tmp/be.out" | sed 's/ $//')"
( best_effort false; echo "rc=$?" ) > "$tmp/be.out" 2> "$tmp/be.err"
expect_match "best_effort: a plain failure is reported" 'failed \(exit 1\): false' "$(cat "$tmp/be.err")"
expect_match "best_effort: ... and returns 0" 'rc=0$' "$(cat "$tmp/be.out")"
# the checker's own commands dying: red / exit 2, never a clean report
sw_file clean 'echo ok'
rc=0; out=$(PATH="$(inject_fail "$tmp/inj-sw-awk" awk 42):$PATH" "$src/scripts/check-exit-swallow.sh" "$sw/clean.sh" 2>&1) || rc=$?
expect_eq    "swallow: awk dies after its report — not clean (exit 2)" "2" "$rc"
rc=0; out=$(PATH="$(inject_fail "$tmp/inj-sw-awk0" awk 42 0):$PATH" "$src/scripts/check-exit-swallow.sh" "$sw/clean.sh" 2>&1) || rc=$?
expect_eq    "swallow: awk dies before printing — not clean (exit 2)" "2" "$rc"
rc=0; out=$(PATH="$(inject_fail "$tmp/inj-sw-git" git 42 all 'ls-files*'):$PATH" "$src/scripts/check-exit-swallow.sh" 2>&1) || rc=$?
expect_eq    "swallow: git ls-files dies after its list — not clean (exit 2)" "2" "$rc"
rc=0; out=$(PATH="$(inject_fail "$tmp/inj-sw-git0" git 42 0 'ls-files*'):$PATH" "$src/scripts/check-exit-swallow.sh" 2>&1) || rc=$?
expect_eq    "swallow: git ls-files dies before listing — not clean (exit 2)" "2" "$rc"
# the repository itself
sw_run
expect_eq    "swallow: every tracked script is clean" "0" "$rc"
[ "$rc" = 0 ] || echo "$out" | head -20 >&2

# ── gates under fault injection (#462) ────────────────────────────────────────────────────────────
# Each external command a gate calls prints (all, none or part of) its real answer and then exits 42. The gate has to end
# non-zero, and its control run — the same fixture, real tools — has to stay green.
# merge-ready.sh: gh is a stub; the wrapper goes around the stub.
ready_dying() {   # ready_dying <stub> <keep> <glob>: gh answers per the stub, but the calls matching <glob> die with exit 42
  local name=$1 keep=$2 glob=$3 w
  w=$(PATH="$tmp/stub-$name/bin:$PATH" inject_fail "$tmp/inj-ready-$name-$RANDOM" gh 42 "$keep" "$glob")
  set +e
  out=$(GH_STUB_DIR="$tmp/stub-$name" PATH="$w:$tmp/stub-$name/bin:$PATH" MERGE_READY_REPO=o/r "$src/scripts/merge-ready.sh" 7 2>&1)
  rc=$?
  set -e
}
stub_gh dying
for call in 'pr view*' 'api repos/o/r/git/ref/heads/*' 'pr checks*' 'run list --workflow ci.yml*' 'run view *' 'api repos/o/r/commits/*'; do
  for keep in all 0; do
    ready_dying dying "$keep" "$call"
    if [ "$rc" = 0 ]; then bad "merge-ready: gh $call prints [$keep] then exits 42, the gate still exits 0 [$out]"; else ok; fi
    if printf '%s' "$out" | grep -q '^READY'; then bad "merge-ready: gh $call dies and the gate still says READY"; else ok; fi
  done
done
stub_gh dyingtree; printf '%s\n%s\n' "$old_main" "$head_sha" > "$tmp/stub-dyingtree/parents"
printf '77 completed success merge-tree pr=7 head=%s base=%s\n' "$head_sha" "$main_sha" > "$tmp/stub-dyingtree/tree_runs"
ready_dying dyingtree all 'run list --workflow merge-tree.yml*'
if [ "$rc" = 0 ]; then bad "merge-ready: gh run list (merge-tree) prints the matching run then exits 42, the gate still exits 0 [$out]"; else ok; fi
ready dyingtree
expect_eq "merge-ready: fault-injection control — the same stale + merge-tree fixture, a healthy gh, is ready" "0" "$rc"
ready dying
expect_eq "merge-ready: fault-injection control — the fresh fixture, a healthy gh, is ready" "0" "$rc"

# red-stage.sh: grep dies
for keep in all 0 1; do
  rc=0; out=$(printf '%s\n' '=== RUN   TestCreate
    order_test.go:42: find calls: want 1 call, got 0
--- FAIL: TestCreate (0.00s)
FAIL' | PATH="$(inject_fail "$tmp/inj-red-$keep" grep 42 "$keep"):$PATH" "$src/scripts/red-stage.sh" -m 'want 1 call.* got 0' 2>&1) || rc=$?
  if [ "$rc" = 0 ]; then bad "red-stage: grep prints [$keep] then exits 42, the gate still calls it an assertion [$out]"; else ok; fi
done

# red-stage.sh, the part that tightened (#462): the body filter used to end in `|| true`, so a grep that died with 2 (not 1 = "no
# line left") after printing its answer still classified the run; now it is a hard failure and nothing is printed. The old class word
# on stdout is the observable that tells the two writings apart.
rc=0; out=$(printf '%s\n' '=== RUN   TestCreate
    order_test.go:42: find calls: want 1 call, got 0
--- FAIL: TestCreate (0.00s)
FAIL' | PATH="$(inject_fail "$tmp/inj-red-ev" grep 2 all '-Ev*'):$PATH" "$src/scripts/red-stage.sh" -m 'want 1 call.* got 0' 2>/dev/null) || rc=$?
expect_eq "red-stage: grep -Ev exits 2 after printing — no class is printed" "" "$out"
if [ "$rc" = 0 ]; then bad "red-stage: grep -Ev exits 2 after printing, the gate still exits 0"; else ok; fi

# affected.sh: git dies in each of its three calls. The three-dot diff failing (no merge base in a shallow clone) must
# not shrink the test matrix: every module runs.
affected_fixture adie
git -C "$repo" checkout -q -b change; touch_file pkgs/a/x.go; commit "chore: adie"
for keep in all 0; do
  w=$(PATH="$PATH" inject_fail "$tmp/inj-aff-3dot-$keep" git 42 "$keep" 'diff --no-renames --name-only origin/main...HEAD')
  rc=0; out=$(cd "$repo" && PATH="$w:$PATH" scripts/affected.sh 2>/dev/null) || rc=$?; out=$(printf '%s\n' "$out" | tr '\n' ' ')
  expect_eq "affected: git diff base...HEAD dies [$keep] — every module is affected" "pkgs/a pkgs/b " "$out"
done
for glob in 'diff --no-renames --name-only HEAD' 'ls-files --others*'; do
  rc=0; out=$(cd "$repo" && PATH="$(inject_fail "$tmp/inj-aff-$RANDOM" git 42 all "$glob"):$PATH" scripts/affected.sh 2>&1) || rc=$?
  if [ "$rc" = 0 ]; then bad "affected: git $glob prints its answer then exits 42, the script still exits 0 [$out]"; else ok; fi
done
rc=0; out=$(cd "$repo" && scripts/affected.sh 2>/dev/null) || rc=$?; out=$(printf '%s\n' "$out" | tr '\n' ' ')
expect_eq "affected: fault-injection control — real git, only pkgs/a changed, a and its dependent b" "pkgs/a pkgs/b " "$out"

# cover-gate.sh: needs go + python3 (the scripts-test job has go; jq and python3 are on the runner image).
cg_fixture() {
  repo="$tmp/cg-$1"; mkdir -p "$repo/scripts" "$repo/pkgs/dom/domain/order"
  cp "$src/scripts/cover-gate.sh" "$repo/scripts/"
  printf 'go 1.21\n\nuse ./pkgs/dom\n' > "$repo/go.work"
  printf 'module example.com/dom\n\ngo 1.21\n' > "$repo/pkgs/dom/go.mod"
  printf 'package order\n\nfunc Add(a, b int) int { return a + b }\n' > "$repo/pkgs/dom/domain/order/o.go"
  printf 'package order\n\nimport "testing"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal("add")\n\t}\n}\n' > "$repo/pkgs/dom/domain/order/o_test.go"
}
cg_run() { rc=0; out=$(cd "$repo" && GOTOOLCHAIN=local "$@" scripts/cover-gate.sh 2>&1) || rc=$?; }
cg_fixture ok; cg_run env
expect_eq    "cover-gate: fixture, a fully covered domain package, is green" "0" "$rc"
expect_match "cover-gate: and it checked the package" 'ok +example.com/dom/domain/order 100%' "$out"
cg_fixture low; printf 'package order\n\nfunc Sub(a, b int) int { return a - b }\n' >> "$repo/pkgs/dom/domain/order/o.go"; cg_run env
expect_eq    "cover-gate: mutant — an uncovered function is red" "1" "$rc"
for keep in all 0; do
  for glob in 'work edit*' 'list *'; do
    cg_fixture "dies-$RANDOM"
    cg_run env "PATH=$(inject_fail "$tmp/inj-cg-$RANDOM" go 42 "$keep" "$glob"):$PATH"
    if [ "$rc" = 0 ]; then bad "cover-gate: go $glob prints [$keep] then exits 42, the gate still exits 0 [$out]"; else ok; fi
  done
done

# payment-upgrade-guard.sh: the home release's database must be the same before and after the drill. kubectl is a fake that
# answers the psql reads; a read that prints part of its answer and dies must not become a shorter, equal snapshot.
pg_fixture() {
  pgd="$tmp/pgguard-$1"; mkdir -p "$pgd/bin"
  cp "$src/deployments/helm/kind/payment-upgrade-guard.sh" "$pgd/"
  printf '#!/usr/bin/env bash\nexit 0\n' > "$pgd/payment-upgrade.sh"   # the inner drill: succeeds and touches nothing
  cat > "$pgd/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
case "$*" in
  *information_schema.tables*) printf 'public.a\npublic.b\npublic.c\n' ;;
  *"count(*)"*) echo 3 ;;
  *pg_namespace*) echo 'schema public' ;;
  *) echo "fake kubectl: unexpected: $*" >&2; exit 99 ;;
esac
FAKE
  chmod +x "$pgd/bin/kubectl"
  mkdir -p "$pgd/home"; : > "$pgd/home/.kind-runner"   # the guard refuses to run outside run.sh's scratch HOME (#444)
}
pg_run() {   # pg_run [<dir of a wrapper that goes first on the PATH>]
  rc=0; out=$(HOME="$pgd/home" KIND_RUNNER_HOME="$pgd/home" PATH="${1:+$1:}$pgd/bin:$PATH" bash "$pgd/payment-upgrade-guard.sh" home 2>&1) || rc=$?
}
pg_fixture ok; pg_run
expect_eq "upgrade guard: fault-injection control — an untouched database passes" "0" "$rc"
for spec in 'all|*information_schema.tables*' '1|*information_schema.tables*' '0|*information_schema.tables*' 'all|*count(*)*' '0|*count(*)*' 'all|*pg_namespace*'; do
  keep=${spec%%|*} glob=${spec#*|}
  pg_fixture "dies-$RANDOM"
  w=$(PATH="$pgd/bin:$PATH" inject_fail "$tmp/inj-pg-$RANDOM" kubectl 42 "$keep" "$glob")
  pg_run "$w"
  if [ "$rc" = 0 ]; then bad "upgrade guard: kubectl $glob prints [$keep] then exits 42, the guard still passes [$out]"; else ok; fi
done

# ── scripts/web-e2e-shards-check.sh: the console-e2e shards are the whole suite (#447) ──────────────────────────────
# A stub pnpm answers `exec playwright test --list [--shard=i/n]` from a 10-test list (shard i = every n-th test), with a fault
# chosen by FAULT. The check must be red for a list that dies half way, a truncated list that exits 0, a shard that drops a
# test, a test listed in two shards, an empty suite; green only when the shards are exactly the suite.
sc="$tmp/shards-check"; mkdir -p "$sc/bin"
for k in 1 2 3 4 5 6 7 8 9 10; do echo "  [chromium] › a$((k % 3)).spec.ts:$k:1 › case $k"; done > "$sc/tests.txt"
cat > "$sc/bin/pnpm" <<'STUB'
#!/usr/bin/env bash
# pnpm exec playwright test --list [--shard=i/n]; FAKE_FAULT picks the fault. Anything else (task's web-install: pnpm install) is a no-op.
[ "${1:-}" = exec ] && [ "${2:-}" = playwright ] || exit 0
PATH="$REAL_PATH"   # the stub itself must not run through the failing wrappers of a case
shard=""; for a in "$@"; do case "$a" in --shard=*) shard=${a#--shard=};; esac; done
if [ -n "$shard" ]; then
  sel=$(awk -v i="${shard%/*}" -v n="${shard#*/}" '(NR - 1) % n == i - 1' "$FAKE_TESTS")
else
  sel=$(cat "$FAKE_TESTS")
fi
total_says=""; code=0
case "${FAKE_FAULT:-}:${shard:-full}" in
  dies-partial:full) echo "Listing tests:"; printf '%s\n' "$sel" | head -3; echo "boom" >&2; exit 42 ;;  # part of the list, then dies
  dies:full)       code=42; echo "boom" >&2 ;;     # a complete, consistent list, but the exit code says it failed
  dies-shard:2/3)  code=42 ;;
  truncated:full)  total_says=$(printf '%s\n' "$sel" | wc -l | tr -d ' '); sel=$(printf '%s\n' "$sel" | head -4) ;;
  drop:2/3)        sel=$(printf '%s\n' "$sel" | tail -n +2) ;;
  dup:1/3)         sel=$(printf '%s\n%s\n' "$sel" "$(printf '%s\n' "$sel" | head -1)") ;;
  empty:full)      sel="" ;;
esac
echo "Listing tests:"
[ -z "$sel" ] || printf '%s\n' "$sel"
echo
echo "Total: ${total_says:-$(printf '%s' "$sel" | grep -c '')} tests in 3 files"
exit "$code"
STUB
chmod +x "$sc/bin/pnpm"
shards_check() { (cd "$tmp" && REAL_PATH="$PATH" PATH="$sc/bin:$PATH" FAKE_TESTS="$sc/tests.txt" FAKE_FAULT="${FAULT:-}" "$src/scripts/web-e2e-shards-check.sh" "$@" 2>&1); }
rc=0; out=$(FAULT= shards_check 3) || rc=$?
expect_eq "shards-check: the shards together are the suite → exit 0" "0" "$rc"
expect_match "shards-check: it says each test once" 'ok: 3 shards = 10 tests, each exactly once' "$out"
rc=0; out=$(FAULT=dies shards_check 3) || rc=$?
expect_eq "shards-check: a --list that prints the whole list but exits 42 is red" "1" "$rc"
expect_match "shards-check: it names the exit code" 'exited 42' "$out"
rc=0; out=$(FAULT=dies-partial shards_check 3) || rc=$?
expect_eq "shards-check: a --list that prints part of the list and exits 42 is red" "1" "$rc"
expect_match "shards-check: the partial list names the exit code too" 'exited 42' "$out"
rc=0; out=$(FAULT=dies-shard shards_check 3) || rc=$?
expect_eq "shards-check: one shard's --list dying half way is red" "1" "$rc"
rc=0; out=$(FAULT=truncated shards_check 3) || rc=$?
expect_eq "shards-check: a truncated list that exits 0 is red" "1" "$rc"
expect_match "shards-check: it says the Total and the lines differ" "Total says '10' but it printed 4" "$out"
rc=0; out=$(FAULT=drop shards_check 3) || rc=$?
expect_eq "shards-check: a shard that drops a test is red" "1" "$rc"
expect_match "shards-check: it says the shards are not the suite" 'are not the whole suite' "$out"
rc=0; out=$(FAULT=dup shards_check 3) || rc=$?
expect_eq "shards-check: a test in two shards is red" "1" "$rc"
expect_match "shards-check: the duplicate is a difference, not a match" 'are not the whole suite' "$out"
rc=0; out=$(FAULT=empty shards_check 3) || rc=$?
expect_eq "shards-check: an empty suite is red, not a match of nothing" "1" "$rc"
# The same faults through the real entry point, `task web-e2e-shards-check -- N` (the Taskfile in this checkout, the stub pnpm first on PATH).
shards_task() { (cd "$tmp" && REAL_PATH="$PATH" PATH="$sc/bin:$PATH" FAKE_TESTS="$sc/tests.txt" FAKE_FAULT="${FAULT:-}" task -t "$src/Taskfile.yml" web-e2e-shards-check -- "$@" 2>&1); }
rc=0; out=$(FAULT= shards_task 3) || rc=$?
expect_eq "shards-check via task: the shards are the suite → exit 0" "0" "$rc"
expect_match "shards-check via task: it says each test once" 'ok: 3 shards = 10 tests, each exactly once' "$out"
rc=0; out=$(FAULT=dies-partial shards_task 3) || rc=$?
expect_eq "shards-check via task: part of the list, then exit 42 → the task exits non-zero" "201" "$rc"
expect_match "shards-check via task: the controlled error names the exit code" 'exited 42' "$out"
rc=0; out=$(FAULT=dies shards_task 3) || rc=$?
expect_eq "shards-check via task: a whole list but exit 42 → the task exits non-zero" "201" "$rc"
rc=0; out=$(FAULT=truncated shards_task 3) || rc=$?
expect_eq "shards-check via task: a truncated list, exit 0 → the task exits non-zero" "201" "$rc"
rc=0; out=$(FAULT=drop shards_task 3) || rc=$?
expect_eq "shards-check via task: a missing test → the task exits non-zero" "201" "$rc"
rc=0; out=$(FAULT=dup shards_task 3) || rc=$?
expect_eq "shards-check via task: a duplicate → the task exits non-zero" "201" "$rc"
# Every external command of the script is checked: a wrapper (first on PATH) runs the real command, then either prints all
# of its output and exits 42 (WRAP_MODE=full) or prints only its first line and exits 42 (WRAP_MODE=partial). The script
# must exit non-zero and never say ok. (grep's exit 1 = no match is the one non-zero it may accept.)
mkdir -p "$sc/wrap"
cat > "$sc/wrap/wrapper" <<'WRAPPER'
#!/usr/bin/env bash
name=$(basename "$0"); real=$(PATH="$REAL_PATH" command -v "$name")
if [ "${WRAP_CMD:-}" != "$name" ]; then exec "$real" "$@"; fi
out=$("$real" "$@"; echo "rc=$?"); out=${out%rc=*}
if [ "${WRAP_MODE:-full}" = partial ]; then printf '%s\n' "$out" | head -1; else printf '%s' "$out"; fi
exit 42
WRAPPER
chmod +x "$sc/wrap/wrapper"
for c in grep sed wc tr cat sort diff seq mktemp; do ln -sf wrapper "$sc/wrap/$c"; done
wrapped_check() { (cd "$tmp" && REAL_PATH="$PATH" PATH="$sc/wrap:$sc/bin:$PATH" FAKE_TESTS="$sc/tests.txt" FAKE_FAULT= WRAP_CMD="$1" WRAP_MODE="$2" "$src/scripts/web-e2e-shards-check.sh" 3 2>&1); }
for c in grep sed wc tr cat sort diff seq mktemp; do
  for m in full partial; do
    rc=0; out=$(wrapped_check "$c" "$m") || rc=$?
    if [ "$rc" = 0 ]; then bad "shards-check: $c printing ($m) then exiting 42 still exits 0"; else ok; fi
    case "$out" in *"ok: 3 shards"*) bad "shards-check: $c ($m) then exit 42 still says ok";; *) ok;; esac
  done
done
# Only the read of the shard indexes fails (the wrappers above fail EVERY call of a tool, which the later cats of the shard lists
# would catch even if this one were swallowed): it prints all of "1 2 3" or only "1", then exits 42.
for keep in all 1; do
  w=$(PATH="$sc/bin:$PATH" inject_fail "$tmp/inj-shard-idx-$keep" cat 42 "$keep" '*indexes*')
  rc=0; out=$( (cd "$tmp" && REAL_PATH="$PATH" PATH="$w:$sc/bin:$PATH" FAKE_TESTS="$sc/tests.txt" FAKE_FAULT= "$src/scripts/web-e2e-shards-check.sh" 3 2>&1) ) || rc=$?
  if [ "$rc" = 0 ]; then bad "shards-check: cat of the indexes prints [$keep] then exits 42, the check still exits 0 [$out]"; else ok; fi
  case "$out" in *"ok: 3 shards"*) bad "shards-check: cat of the indexes ($keep) then exit 42 still says ok";; *) ok;; esac
  expect_match "shards-check: the index read names its failure ($keep)" 'cat of the shard indexes exited 42' "$out"
done
rc=0; out=$(WRAP_CMD=none wrapped_check none full) || rc=$?
expect_eq "shards-check: the wrappers that do not fail change nothing → exit 0" "0" "$rc"
rc=0; out=$(FAULT= shards_check) || rc=$?
expect_eq "shards-check: no shard count is a usage error" "2" "$rc"
rc=0; out=$(FAULT= shards_check 0) || rc=$?
expect_eq "shards-check: zero shards is a usage error" "2" "$rc"

echo "scripts/test.sh: $pass passed, $fail failed"
[ "$fail" = 0 ]
