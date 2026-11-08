#!/usr/bin/env bash
# Tests for the CI / release decision scripts whose wrong answer is silent: scripts/ci-changes.sh (which ci.yml jobs a change
# runs; a wrong rule skips a job and CI stays green), scripts/affected.sh (which modules the test matrix runs),
# scripts/releasable.sh (does a change cut a release), scripts/cosign-sign-blob.sh (no empty signature is uploaded), and the
# wiring nothing else checks: the k6-auth shards cover the whole matrix, only push / workflow_dispatch write a cache, the
# release image lists agree, MODULES follows go.work, the console OIDC env gate.
#
#   scripts/test.sh          # task scripts-test; CI's scripts-test job
#
# Each case builds a throwaway git repo with scripts/ copied in and runs the copies there
# (ci-changes.sh cd's to its own repo root).
# Plain bash 3.2 (macOS) and 5 (ubuntu): no mapfile, no associative arrays.
set -euo pipefail
# abs_dir_of <path of a file> [relative subdirectory]: the absolute directory of that file, without `cd … && pwd` and without dirname inside $(…) (#470, #472)
abs_dir_of() { local p=${1%/*}; [ "$p" != "$1" ] || p=.; ( cd "$p${2:+/$2}" && pwd ); }
src=$(abs_dir_of "$0" ..) || exit 1
tmp=$(mktemp -d) || exit 1
# The fixtures must not inherit the developer's signing / hook settings.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
# abort_note <status> <line> <command>: an exit with a non-zero status BEFORE the summary line says where it happened. A bare `|| exit 1` in a helper (the rewrites of #472
# are full of them) or a set -e abort used to end the suite silently: CI showed no FAIL line and no summary, and nothing to look at. suite_done is set just before the summary.
suite_done=""
abort_note() {
  if [ "$1" != 0 ] && [ -z "$suite_done" ]; then echo "scripts/test.sh: stopped with exit $1 before the summary, at line $2: $3" >&2; fi
}
trap 'abort_note "$?" "$LINENO" "$BASH_COMMAND"; rm -rf "$tmp"' EXIT
fail=0 pass=0
uid_now=$(id -u) || exit 1   # the cells that need an unreadable file are skipped for root
. "$src/scripts/best-effort.sh"    # best_effort: cleanup that must not decide a verdict, and says so when it fails (#462)
# le1 <command…>: a filter whose exit 1 means "found nothing" (grep -c / -m1): 0 and 1 are a success, 2+ is the command's own failure and stays one.
le1() { local rc=0; "$@" || rc=$?; [ "$rc" -le 1 ] || return "$rc"; return 0; }
# count_file <file>: the number in a counter file; 0 when the fake never created it (no file = never called); a file that cannot be read fails
count_file() { [ -f "$1" ] || { echo 0; return 0; }; cat "$1"; }
in_dir() { local d=$1; shift; ( cd "$d" && "$@" ); }   # run a command in another directory without moving this shell (capture -2 runs its command in this shell)

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
touch_file() { local d=${1%/*}; [ "$d" != "$1" ] || d=.; mkdir -p "$repo/$d"; echo "${2:-x}" > "$repo/$1"; }   # ${1%/*}, not dirname inside $(…) (#472)
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
# eq_of <name> <want> <command…> (#472): expect_eq against the STDOUT of a command, the command written as words instead of
# inside "$(…)" in an argument, where its status is lost (a command that printed half of its answer and then died would be compared). The command runs in a
# subshell (as $(…) did: no state leaks out) with errexit as it was; capture keeps stdout and the status apart, and a non-zero status is a failed case, never a value.
subshell() { ( "$@" ); }
eq_of() { local _n=$1 _w=$2 _o _r; shift 2; capture _o _r subshell "$@" || exit 1; if [ "$_r" = 0 ]; then expect_eq "$_n" "$_w" "$_o"; else bad "$_n: [$*] exited $_r"; fi; }
# count_lines <grep -c args>: the number of matching lines; 1 (no match) is 0, anything above is a failure (exit 2). Meant for an ASSIGNMENT (n=$(… | count_lines x)): inside an argument the exit never reaches set -e (#462)
count_lines() { local rc=0; grep -c "$@" || rc=$?; if [ "$rc" -gt 1 ]; then echo "test.sh: grep failed with exit $rc in count_lines $*" >&2; exit 2; fi; }   # grep -c prints the count (0 too) and exits 1 on "none": that is a count, not a failure

# ── ci-changes.sh ───────────────────────────────────────────────────────────────
all_jobs="gen_freshness lint work_sync coverage build breaking release_snapshot actions_resolve web console_e2e console_image helm scripts_test helm_kind helm_kind_payment helm_kind_keda helm_kind_target k6 k6_auth k6_wire"
# ci_changes_jobs: runs scripts/ci-changes.sh in $repo and prints the jobs set to true. The script's exit code and the formatting
# pipeline are each checked: CI-CHANGES-FAILED when ci-changes.sh dies (even after printing a plausible set), FORMAT-FAILED when sed / tr die.
ci_changes_jobs() {
  local out res
  out=$(in_repo scripts/ci-changes.sh) || { echo CI-CHANGES-FAILED; return 1; }
  res=$(printf '%s\n' "$out" | sed -n 's/^run_\(.*\)=true$/\1/p' | tr '\n' ' ' | sed 's/ $//') || { echo FORMAT-FAILED; return 1; }
  echo "$res"
}
# jobs_on <name> <files...>: commits the files on a branch off main and returns the jobs set to true.
jobs_on() {
  local name=$1; shift
  fixture "$name"
  git -C "$repo" checkout -q -b change
  for f in "$@"; do touch_file "$f"; done
  commit "chore: $name"
  ci_changes_jobs
}
# Shorthands for the sets below (#328): the Go checks every Go change runs, and what only product code adds.
go_checks="gen_freshness lint work_sync coverage"
go_prod_pre="build console_e2e helm_kind helm_kind_payment"; go_prod="$go_prod_pre k6_wire"
# The Grafana dashboard and alert rules are inputs of helm-check's check-observability.py (#367 slice 5c): a metric-name typo in a rule that never fires is caught there, so a change to them runs helm and nothing else.
eq_of "grafana provisioning runs helm-check only" "helm" jobs_on grafana deployments/docker/grafana/provisioning/alerting.yaml
eq_of "a _test.go alone runs only the Go checks" "$go_checks" jobs_on gotest pkgs/ordering/domain/order/order_test.go
# A test helper that is not named *_test.go is product code: it can be imported by a binary.
eq_of "a non-_test.go test helper is product code" \
  "$go_checks build console_e2e helm_kind helm_kind_payment helm_kind_keda k6_wire" \
  jobs_on pgtest pkgs/platform/postgres/pgtest/pgtest.go
eq_of "a _test.go next to a product file changes nothing about the product file" \
  "$go_checks $go_prod" \
  jobs_on gomixed pkgs/ordering/domain/order/order.go pkgs/ordering/domain/order/order_test.go

# Product Go outside the auth / k6 / worker paths: no release-snapshot, console-image, keda, k6, k6-auth.
eq_of "go runs the Go checks, kind and k6-wire" \
  "$go_checks $go_prod" \
  jobs_on go pkgs/ordering/domain/order/order.go
gomod_jobs="$go_checks build release_snapshot console_e2e helm_kind helm_kind_payment helm_kind_keda k6 k6_wire"
eq_of "go.mod counts as go, and as a release / keda / k6 input" "$gomod_jobs" jobs_on gomod pkgs/ordering/go.mod
gowork_jobs="$go_checks build release_snapshot console_e2e helm_kind helm_kind_payment helm_kind_keda helm_kind_target k6 k6_wire"
eq_of "go.work likewise, and a helm-kind-target input" "$gowork_jobs" jobs_on gowork go.work
eq_of "proto is go + breaking + web, console-image included" \
  "$go_checks build breaking web console_e2e console_image helm_kind helm_kind_payment k6_wire" \
  jobs_on proto protos/ordering/v1/service.proto
eq_of "web only: no lint, no k6, no keda" \
  "gen_freshness web console_e2e console_image helm_kind helm_kind_payment" \
  jobs_on web web/apps/console/src/routes/orders.tsx
eq_of "helm only" "helm helm_kind helm_kind_payment helm_kind_keda" jobs_on helm deployments/helm/x-monorepo/values.yaml
# helm-kind-target (#446) runs deployments/helm/kind/target-kubeconfig-test.sh, which executes the kind runner and the Taskfile's
# helm-kind-cluster / helm-kind-down. Its inputs are named in ci-changes.sh (target_src); each one alone must run it, and the helm
# jobs next to it (everything under deployments/helm/ is a helm input). Anything else must not: it creates two kind clusters.
kind_target_jobs="helm helm_kind helm_kind_payment helm_kind_keda helm_kind_target"
kind_target_sh_jobs="helm helm_kind helm_kind_payment helm_kind_keda helm_kind_target"   # a *.sh no longer runs scripts-test
eq_of "kind/run.sh alone runs helm-kind-target"           "$kind_target_sh_jobs" jobs_on tgtrun deployments/helm/kind/run.sh
eq_of "kind/lock.sh alone runs helm-kind-target"          "$kind_target_sh_jobs" jobs_on tgtlock deployments/helm/kind/lock.sh
eq_of "kind/shim/kubectl alone runs helm-kind-target"     "$kind_target_jobs" jobs_on tgtshimk deployments/helm/kind/shim/kubectl
eq_of "kind/shim/helm alone runs helm-kind-target"        "$kind_target_jobs" jobs_on tgtshimh deployments/helm/kind/shim/helm
eq_of "kind/target-kubeconfig-test.sh alone runs helm-kind-target" "$kind_target_sh_jobs" jobs_on tgtkc deployments/helm/kind/target-kubeconfig-test.sh
eq_of "kind/target-test.sh alone runs helm-kind-target"   "$kind_target_sh_jobs" jobs_on tgtt deployments/helm/kind/target-test.sh
eq_of "kind/target-isolation-test.sh alone runs helm-kind-target" "$kind_target_sh_jobs" jobs_on tgtiso deployments/helm/kind/target-isolation-test.sh
# the Taskfile is where helm-kind-cluster / helm-kind-down live: ci (all_jobs, above) already covers it. Not inputs:
eq_of "the chart does not run helm-kind-target"           "helm helm_kind helm_kind_payment helm_kind_keda" jobs_on tgtchart deployments/helm/x-monorepo/templates/deployment.yaml
eq_of "kind/payment-flow.sh does not run helm-kind-target" "helm helm_kind helm_kind_payment helm_kind_keda" jobs_on tgtpay deployments/helm/kind/payment-flow.sh
eq_of "kind/values.yaml does not run helm-kind-target"    "helm helm_kind helm_kind_payment helm_kind_keda" jobs_on tgtvals deployments/helm/kind/values.yaml
eq_of "kind/diag.sh does not run helm-kind-target"        "helm helm_kind helm_kind_payment helm_kind_keda" jobs_on tgtdiag deployments/helm/kind/diag.sh
eq_of "kind/runner-home-test.sh does not run helm-kind-target (it is the fake-tool drills' helper)" "helm helm_kind helm_kind_payment helm_kind_keda" jobs_on tgthome deployments/helm/kind/runner-home-test.sh
eq_of "plain product Go does not run helm-kind-target"    "$go_checks $go_prod" jobs_on tgtgo pkgs/ordering/domain/order/order.go
eq_of "web does not run helm-kind-target"                 "gen_freshness web console_e2e console_image helm_kind helm_kind_payment" jobs_on tgtweb web/apps/console/src/routes/orders.tsx
eq_of "a run.sh look-alike elsewhere does not run helm-kind-target (only scripts-test, for the *.sh)" "" jobs_on tgtlook docs/run.sh scripts_old/shim/kubectl
eq_of "dex config runs console-e2e" "console_e2e" jobs_on dexcfg deployments/docker/dex/config.yaml
# A rename is a delete plus an add. `git diff --name-only` pairs them and prints only the new path, so
# helper.go -> helper_test.go would look test-only while it removed a product file: ci-changes.sh
# diffs with --no-renames and has_prod sees both paths. rename_jobs_on <name> <old> <new>.
rename_jobs_on() {
  local name=$1 old=$2 new=$3
  fixture "$name"
  touch_file "$old" "package x"; commit "chore: $old"
  git -C "$repo" update-ref refs/remotes/origin/main main
  git -C "$repo" checkout -q -b change
  new_dir=${new%/*}; [ "$new_dir" != "$new" ] || new_dir=.
  mkdir -p "$repo/$new_dir"
  git -C "$repo" mv "$old" "$new"
  commit "chore: rename"
  ci_changes_jobs
}
eq_of "renaming a product file to _test.go removed a product input" \
  "$go_checks $go_prod" rename_jobs_on torename pkgs/ordering/helper.go pkgs/ordering/helper_test.go
# helm-kind-target: ci-changes.sh diffs with --no-renames, so a runner file renamed OUT of its directory is a deletion of an input and
# still runs the job (a rename-aware diff prints only the new path, docs/run.sh, which no rule names). The same for every input, and for
# a rename INTO one.
eq_of "moving kind/run.sh out still runs helm-kind-target" \
  "$kind_target_sh_jobs" rename_jobs_on movekindrun deployments/helm/kind/run.sh docs/run.sh
eq_of "moving kind/lock.sh out still runs helm-kind-target" \
  "$kind_target_sh_jobs" rename_jobs_on movekindlock deployments/helm/kind/lock.sh docs/lock.sh
eq_of "moving kind/shim/kubectl out still runs helm-kind-target" \
  "$kind_target_jobs" rename_jobs_on movekindshim deployments/helm/kind/shim/kubectl docs/kubectl
eq_of "moving kind/target-kubeconfig-test.sh out still runs helm-kind-target" \
  "$kind_target_sh_jobs" rename_jobs_on movekindtgt deployments/helm/kind/target-kubeconfig-test.sh docs/target-kubeconfig-test.sh
eq_of "moving go.work out still runs helm-kind-target (and the rest of go.work's jobs)" \
  "$gowork_jobs" rename_jobs_on movegowork go.work docs/go.work
eq_of "moving a file INTO kind/run.sh's place runs helm-kind-target" \
  "$kind_target_sh_jobs" rename_jobs_on movekindin docs/run.sh deployments/helm/kind/run.sh
# a rename inside the kind directory between two non-inputs is not an input either way
eq_of "renaming kind/smoke.sh to kind/smoke2.sh does not run helm-kind-target" \
  "helm helm_kind helm_kind_payment helm_kind_keda" rename_jobs_on movekindsmoke deployments/helm/kind/smoke.sh deployments/helm/kind/smoke2.sh
# ...but renaming a non-input onto an input name is one
eq_of "renaming kind/smoke.sh to kind/target-smoke.sh runs helm-kind-target" \
  "$kind_target_sh_jobs" rename_jobs_on movekindtosmoke deployments/helm/kind/smoke.sh deployments/helm/kind/target-smoke.sh
eq_of "k6 scripts: all three k6 jobs"  "k6 k6_auth k6_wire" jobs_on k6 tests/k6/orders.js
eq_of "platform/server: k6, k6-auth, keda, k6-wire" \
  "$go_checks $go_prod_pre helm_kind_keda k6 k6_auth k6_wire" jobs_on server pkgs/platform/server/limits.go
eq_of "identity: k6-auth only"      "$go_checks $go_prod_pre k6_auth k6_wire" jobs_on identity pkgs/identity/provider.go
eq_of "Dockerfile is an image change" \
  "release_snapshot console_image helm_kind helm_kind_payment helm_kind_keda" \
  jobs_on image deployments/docker/Dockerfile.rpc
eq_of ".goreleaser.yaml is an image change, and a scripts-test input (the release image list cases read it)" \
  "release_snapshot console_image scripts_test helm_kind helm_kind_payment helm_kind_keda" \
  jobs_on goreleaser .goreleaser.yaml
eq_of "release.yml runs release-snapshot too" "release_snapshot actions_resolve" jobs_on relyml .github/workflows/release.yml
eq_of "ci.yml runs everything"   "$all_jobs" jobs_on ciyml .github/workflows/ci.yml
eq_of "Taskfile runs everything" "$all_jobs" jobs_on taskfile Taskfile.yml
eq_of "scripts/ runs everything" "$all_jobs" jobs_on scripts scripts/new.sh

fixture ciall
got=$(in_repo env CI_ALL=1 scripts/ci-changes.sh | sed -n 's/^run_\(.*\)=true$/\1/p' | tr '\n' ' ' | sed 's/ $//') || exit 1
expect_eq "CI_ALL=1 runs everything" "$all_jobs" "$got"
# Every flag is a job's `if:` in ci.yml, declared once as an output of `affected` (a flag nobody reads skips nothing).
for j in $all_jobs; do
  _c220a=$(count_lines "^      run_$j: \${{ steps.jobs.outputs.run_$j }}\$" "$src/.github/workflows/ci.yml") || exit 1; expect_eq "ci.yml declares run_$j as an output of affected" 1 "$_c220a"
  _c221a=$(count_lines "^    if: needs.affected.outputs.run_$j == 'true'\$" "$src/.github/workflows/ci.yml") || exit 1; expect_eq "ci.yml has exactly one job gated on run_$j"       1 "$_c221a"
done
# ci_job <name>: the lines of that top-level job in ci.yml.
ci_job() { awk -v n="  $1:" '$0 == n {f=1; next} /^  [a-z0-9-]+:/ || /^  #/ {f=0} f' "$src/.github/workflows/ci.yml"; }
# ci_count <job> <grep -c args…>: how many lines of that job match. The exit codes are checked on their own, never read off the printed
# value: CI-JOB-FAILED when awk dies (even after printing), CI-JOB-EMPTY when the job is not there (a "0 matches" would pass on a job that
# does not exist), COUNT-FAILED when grep fails with anything but "no match" (#446 r0: `$(ci_count x y)` stayed green on a grep that
# printed 1 and then exited 42).
ci_count() {
  local j=$1 body n
  shift
  body=$(ci_job "$j") || { echo CI-JOB-FAILED; return 1; }
  [ -n "$body" ] || { echo CI-JOB-EMPTY; return; }
  n=$(printf '%s\n' "$body" | le1 grep -c "$@") || { echo COUNT-FAILED; return 1; }
  echo "$n"
}
# helm-kind, helm-kind-keda and k6-auth / k6-wire are jobs of their own, not matrix legs: a skipped one
# leaves a skipped check, a dynamic matrix would leave none (#328).
_c228a=$(count_lines 'task: \[helm-kind' "$src/.github/workflows/ci.yml") || exit 1; expect_eq "no matrix over the kind tasks" 0 "$_c228a"
# helm-kind is the first half of task helm-kind and helm-kind-payment the second, each on its own cluster (#446); task helm-kind itself is the
# laptop's "everything on one cluster", so no CI job runs it (it would run both halves again).
eq_of "helm-kind job runs task helm-kind-core"            1 ci_count helm-kind '^      - run: task helm-kind-core$'
eq_of "helm-kind-payment job runs task helm-kind-payment" 1 ci_count helm-kind-payment '^      - run: task helm-kind-payment$'
whole_kind=$(count_lines '^      - run: task helm-kind$' "$src/.github/workflows/ci.yml") || exit 1   # empty (so the assertion fails) when grep dies: count_lines stops the subshell
expect_eq "no ci.yml job runs the whole task helm-kind"       0 "$whole_kind"
eq_of "helm-kind-target job runs target-kubeconfig-test.sh" 1 ci_count helm-kind-target '^      - run: deployments/helm/kind/target-kubeconfig-test.sh$'
eq_of "helm-kind no longer runs target-kubeconfig-test.sh (its own job)" 0 ci_count helm-kind 'target-kubeconfig-test'
eq_of "helm-kind-payment does not run target-kubeconfig-test.sh"        0 ci_count helm-kind-payment 'target-kubeconfig-test'
# helm-kind-target needs neither node, helm nor k6 (docs/ci.md says so)
eq_of "helm-kind-target installs no node / helm / k6" 0 ci_count helm-kind-target -E 'setup-node|setup-helm|setup-k6|pnpm'
eq_of "helm-kind-target has no matrix"  0 ci_count helm-kind-target 'matrix'
# #448: k6-auth is one job with a static matrix of shards, K6_AUTH_SHARD=<i>/<n>. The shards must be exactly
# 1/n … n/n, and their cells together must be the whole list, each once — a cell in no shard would never run.
m=$src/tests/k6/auth-matrix.sh
capture all_cells all_cells_rc env K6_AUTH_LIST=1 "$m" || exit 1
[ "$all_cells_rc" = 0 ] || { echo "scripts/test.sh: $m K6_AUTH_LIST=1 failed" >&2; exit 1; }
got=$(printf '%s\n' "$all_cells" | wc -l | tr -d ' ') || exit 1
shards=$(ci_job k6-auth | sed -n 's/^        shard: \[\(.*\)\]$/\1/p' | tr -d '" ' | tr ',' ' ') || exit 1
n=${shards##*/}
want=""; for ((i = 1; i <= n; i++)); do want="$want $i/$n"; done
expect_eq "k6-auth shards are 1/n … n/n" "${want# }" "$shards"
union=""
for ((i = 1; i <= n; i++)); do
  capture part part_rc env K6_AUTH_LIST=1 K6_AUTH_SHARD=$i/$n "$m" || exit 1
  [ "$part_rc" = 0 ] || { echo "scripts/test.sh: $m shard $i/$n failed" >&2; exit 1; }
  expect_match "auth matrix shard $i/$n is not empty" '.' "$part"
  union="$union
$part"
done
cells_sorted=$(printf '%s\n' "$all_cells" | sort) || exit 1
union_sorted=$(printf '%s\n' "$union" | sed '/^$/d' | sort) || exit 1
expect_eq "the union of the k6-auth shards is the whole cell list, each cell once" \
  "$cells_sorted" "$union_sorted"
plan="$src/.github/actions/setup/plan.sh"
# plan_out <event> <ref> <default branch> <scope> <output>: one output of plan.sh; PLAN-FAILED / NO-KEY when the script failed or lacks the key,
# so that an expectation of "" cannot pass on a crashed script.
plan_out() {
  local o rc
  EVENT_NAME=$1 REF=$2 DEFAULT_BRANCH=$3 SCOPE=$4 GITHUB_OUTPUT=/dev/stdout capture o rc "$plan" || return 1
  if [ "$rc" != 0 ]; then echo PLAN-FAILED; return; fi
  n_key=$(printf '%s\n' "$o" | count_lines "^$5=") || return 1
  if [ "$n_key" != 1 ]; then echo NO-KEY; return; fi
  printf '%s\n' "$o" | sed -n "s/^$5=//p"
}
# GitHub (docs "Cache access for low-trust workflow triggers"): only push / workflow_dispatch / repository_dispatch / delete /
# registry_package / page_build / schedule may create caches in the default branch's scope; workflow_run, pull_request_target,
# issue_comment ... are read-only there. This repo writes from push and workflow_dispatch only.
eq_of "plan: a pull request never writes"                 false plan_out pull_request refs/pull/7/merge main lint write
eq_of "plan: pull_request_target on main never writes"    false plan_out pull_request_target refs/heads/main main lint write
eq_of "plan: workflow_run on main never writes"           false plan_out workflow_run refs/heads/main main lint write
eq_of "plan: schedule on main does not write"             false plan_out schedule refs/heads/main main lint write
eq_of "plan: issue_comment on main never writes"          false plan_out issue_comment refs/heads/main main lint write
# the web job needs go on PATH (task evaluates `go work edit` on every run) but compiles nothing; lint's own golangci cache must not save on a PR
optouts=$(count_lines 'go-cache: "false"' "$src/.github/workflows/ci.yml") || exit 1
expect_eq "ci.yml: only the web and helm-kind-target jobs opt out of the Go cache" 2 "$optouts"
eq_of "ci.yml: the helm-kind-target job opts out of the Go cache (it compiles nothing, #446)" 1 ci_count helm-kind-target 'go-cache: "false"'
eq_of "ci.yml: the helm-kind-target job still runs the setup action (task needs go)" 1 ci_count helm-kind-target 'uses: ./.github/actions/setup$'
# A branch behind main must see only its own files (three-dot diff), not main's newer ones.
fixture behind
git -C "$repo" checkout -q -b change
touch_file tests/k6/x.js; commit "test: k6"
git -C "$repo" checkout -q main
touch_file pkgs/x/a.go; commit "feat: go on main"
git -C "$repo" update-ref refs/remotes/origin/main main
git -C "$repo" checkout -q change
got=$(in_repo scripts/ci-changes.sh | sed -n 's/^run_\(.*\)=true$/\1/p' | tr '\n' ' ' | sed 's/ $//') || exit 1
expect_eq "branch behind main sees only its own files" "k6 k6_auth k6_wire" "$got"
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
eq_of "affected: dependents follow"              "pkgs/a pkgs/b" affected_on adep pkgs/a/x.go
eq_of "affected: ci.yml is every module"         "pkgs/a pkgs/b" affected_on aci .github/workflows/ci.yml
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
eq_of "affected: a file moved out of a module still affects it" "pkgs/a pkgs/b" affected_rename amove
affected_fixture aall
got=$(in_repo scripts/affected.sh --all | tr '\n' ' ' | sed 's/ $//') || exit 1
expect_eq "affected: --all ignores the diff"         "pkgs/a pkgs/b" "$got"
# ── releasable.sh ───────────────────────────────────────────────────────────────
fixture releasable
touch_file README.md changed; touch_file docs/guide.md; touch_file pkgs/ordering/README.md
touch_file deployments/helm/x-monorepo/Chart.yaml; touch_file web/apps/console/e2e/a.spec.ts
touch_file pkgs/ordering/domain/order/order_test.go; touch_file .github/workflows/ci.yml
touch_file scripts/x.sh; touch_file Taskfile.yml; touch_file tests/k6/a.js
touch_file deployments/docker/docker-compose.yaml; touch_file deployments/docker/grafana/d.json
touch_file deployments/docker/dex/config.yaml
commit "docs: nothing shipped"
eq_of "docs / chart / tests / CI are not releasable" "" in_repo scripts/releasable.sh
touch_file pkgs/ordering/domain/order/order.go; touch_file deployments/docker/Dockerfile.rpc
touch_file configs/config.example.yaml; touch_file web/apps/console/src/main.tsx; touch_file .goreleaser.yaml
commit "feat: shipped"
eq_of "shipped files are releasable" \
  ".goreleaser.yaml
configs/config.example.yaml
deployments/docker/Dockerfile.rpc
pkgs/ordering/domain/order/order.go
web/apps/console/src/main.tsx" \
  in_repo scripts/releasable.sh
eq_of "explicit range sees only that range" "" in_repo scripts/releasable.sh HEAD~2 HEAD~1
# A rename between a shipped file and a _test.go still changes what ships (--no-renames: the shipped path is listed
# either way; the pathspec already splits the pair, this pins it).
fixture relrename
touch_file pkgs/ordering/helper.go "package x"; touch_file pkgs/ordering/other_test.go "package x"
commit "chore: files at the tag"
git -C "$repo" tag -f v0.1.0 >/dev/null
git -C "$repo" mv pkgs/ordering/helper.go pkgs/ordering/helper_test.go
git -C "$repo" mv pkgs/ordering/other_test.go pkgs/ordering/other.go
commit "chore: swap"
eq_of "renames between product and _test.go list the product path" \
  "pkgs/ordering/helper.go
pkgs/ordering/other.go" in_repo scripts/releasable.sh
# ── cosign-sign-blob.sh ─────────────────────────────────────────────────────────
# .goreleaser.yaml's `signs.cmd` (#215 option C): cosign sign-blob, re-signed at most twice more and
# only when the failure is a Rekor 409. Fake cosign, scripted per attempt by FAKE_SEQ (space
# separated): ok = write the bundle, 409 = Rekor conflict on stderr + exit 1, err = another failure,
# empty = exit 0 with an empty bundle, none = exit 0 with no bundle. Offline, no sleeping.
signfake="$tmp/cosign-sign"; mkdir -p "$signfake"
cat > "$signfake/cosign" <<'FAKE'
#!/usr/bin/env bash
n=$(( $(cat "$FAKE_COUNT" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$FAKE_COUNT"
art=$3; echo "$*" >> "$FAKE_ARGS"; echo "token=${SIGSTORE_ID_TOKEN:-}" >> "$FAKE_ARGS"
for a in "$@"; do case $a in --bundle=*) bundle=${a#--bundle=} ;; esac; done
[ -s "$bundle" ] && echo "STALE: attempt $n found a bundle left by an earlier one" >> "$FAKE_ARGS"
set -- $FAKE_SEQ; step=${!n:-err}
case $step in
  ok)    echo '{"fake":"bundle"}' > "$bundle" ;;
  409)   echo "Error: signing $art: [POST /api/v1/log/entries][409] createLogEntryConflict {\"code\":409,\"message\":\"an equivalent entry already exists\"}" >&2; echo partial > "$bundle"; exit 1 ;;
  empty) : > "$bundle" ;;
  none)  : ;;
esac
FAKE
chmod +x "$signfake/cosign"
# sign_blob <name> "<FAKE_SEQ>": runs the wrapper with goreleaser's args; sets sb_rc, sb_out, sb_tries, sb_bundle.
sign_blob() {
  local d="$tmp/sb-$1" art=checksums.txt; mkdir -p "$d"; echo artifact > "$d/$art"
  : > "$d/args"; rm -f "$d/count"
  sb_bundle="$d/checksums.txt.sigstore.json"
  set +e
  sb_rc=0
  capture -2 sb_out sb_rc in_dir "$src" env PATH="$signfake:$PATH" FAKE_COUNT="$d/count" FAKE_ARGS="$d/args" FAKE_SEQ="$2" SIGSTORE_ID_TOKEN=secret-oidc-token \
    COSIGN_SIGN_RETRY_DELAY=0 scripts/cosign-sign-blob.sh sign-blob --bundle="$sb_bundle" "$d/$art" --yes || exit 1
  set -e
  sb_tries=$(count_file "$d/count") || exit 1; sb_args=$d/args   # no counter file = the fake was never called
}
sign_blob retry "409 ok"
if grep -q STALE "$sb_args"; then bad "sign: retry started on the previous attempt's bundle"; else ok; fi
sign_blob empty "empty"
if [ "$sb_rc" != 0 ]; then ok; else bad "sign: success with an empty bundle must fail"; fi
expect_eq    "sign: empty bundle is not retried"           1 "$sb_tries"
if [ ! -s "$sb_bundle" ]; then ok; else bad "sign: empty bundle case left a non-empty bundle"; fi
sign_blob none "none"
if [ "$sb_rc" != 0 ]; then ok; else bad "sign: success with no bundle must fail"; fi
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
  eq_of "envsh accepts $good" 0 envsh_rc "$good" x-console
done
for badv in 'https://idp.example.com:65536/dex' 'https://idp.example.com:99999' 'https://idp.example.com:0/dex' 'https://10.0.0.256/dex' 'https://idp.example.com.123/dex' 'https://idp.example.com.0x1' 'https://idp.example.com.0XFF/dex' 'https://0x7f.0x1' 'https://idp.example.com.0x' 'https://idp..example.com' 'https://-idp.example.com' 'https://idp.example.com/dex?x=1' 'https://u@idp.example.com' 'ftp://idp.example.com' 'https://idp.example.com/de x'; do
  eq_of "envsh refuses $badv" 1 envsh_rc "$badv" x-console
done
eq_of "envsh: nothing set is OIDC off, not an error"    0 envsh_rc '' ''
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
go_images=$(images_in "$src/.goreleaser.yaml") || exit 1
apps=$(sed -n 's/^  APPS: //p' "$src/Taskfile.yml" | words) || exit 1
with_console=$(printf '%s console' "$go_images" | words) || exit 1
expect_match "release images: .goreleaser.yaml lists images (positive control)" "rpc" "$go_images"
expect_eq "release images: every app in Taskfile APPS ships a goreleaser image, and nothing else does" "$apps" "$go_images"
got=$(sed -n 's/^for app in \(.*\); do$/\1/p' "$src/scripts/release-verify.sh" | words) || exit 1
expect_eq "release images: release-verify.sh checks each image's signature" "$with_console" "$got"
# ── MODULES is derived from go.work; check-modules.sh refuses go.work paths it cannot carry (#413) ──────────────
# lint / test / cover / tidy loop over Taskfile.yml's MODULES. It used to be a second hand-kept list, and a module only
# in go.work was silently never linted or tested (apps/payment-worker, #412); five rounds of a gate comparing the two
# each hit a path the text channel between them could not carry. MODULES is now `go work edit -json | jq` of go.work,
# so a module added there is iterated by every per-module task, and scripts/check-modules.sh (task check-work-sync,
# CI work-sync) only refuses the go.work shapes the derivation cannot carry. Skipped without go / jq / task on PATH like
# the other optional cases — but never in CI ($CI is set on GitHub Actions), where a missing tool must be a red run, not
# a skipped gate (the scripts-test job runs ./.github/actions/setup for go + task; jq is on the ubuntu runner image).
if [ -n "${CI:-}" ] || { command -v go >/dev/null 2>&1 && command -v task >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; }; then
# MODULES is derived from go.work: the real Taskfile.yml and a go.work with one more `use`, run through `task --dry`
# (dry still resolves vars). A module added to go.work only is iterated by every per-module task.
d="$tmp/modules-derived"; mkdir -p "$d"
cp "$src/Taskfile.yml" "$d/Taskfile.yml"
cp -R "$src/scripts" "$d/scripts"
cp "$src/go.work" "$d/go.work"
printf '\nuse ./apps/brand-new\n' >> "$d/go.work"
for t in lint test tidy; do
  rc=0; capture -2 out rc in_dir "$d" task --dry "$t" || exit 1
  expect_match "derived modules: task $t runs for an existing module (positive control)" 'cd pkgs/kernel' "$out"
  expect_match "derived modules: task $t iterates a module only go.work lists" 'cd apps/brand-new' "$out"
done
# A go that cannot read go.work must fail the task, not leave MODULES empty (every per-module loop would iterate nothing).
d="$tmp/modules-derived"   # the fixture built above (a sibling case used to leave $d pointing elsewhere)
mkdir -p "$tmp/failing-go"; printf '#!/bin/sh\necho "go: stub failure" >&2\nexit 1\n' > "$tmp/failing-go/go"; chmod +x "$tmp/failing-go/go"
rc=0; capture -2 out rc in_dir "$d" env PATH="$tmp/failing-go:$PATH" task --dry lint || exit 1
expect_eq "derived modules: a failing go work edit fails the task instead of an empty MODULES" "1" "$rc"
fi
# affected.sh: the three-dot diff failing (no merge base in a shallow clone) must
# not shrink the test matrix: every module runs.
affected_fixture adie
git -C "$repo" checkout -q -b change; touch_file pkgs/a/x.go; commit "chore: adie"
# the real git prints the list and THEN dies: that answer is not trusted, every module runs
w="$tmp/inj-aff-3dot"; mkdir -p "$w"
real_git=$(type -P git) || exit 1
printf '#!/bin/sh\nif [ "$*" = "diff --no-renames --name-only origin/main...HEAD" ]; then %s "$@"; exit 42; fi\nexec %s "$@"\n' "$real_git" "$real_git" > "$w/git"; chmod +x "$w/git"
rc=0; capture out rc in_dir "$repo" env PATH="$w:$PATH" scripts/affected.sh 2>/dev/null || exit 1; out=$(printf '%s\n' "$out" | tr '\n' ' ') || exit 1
expect_eq "affected: git diff base...HEAD prints the list and dies — every module is affected" "pkgs/a pkgs/b " "$out"
fn_text() { # <file> <name>: the function, from its first line to its closing brace (indented or not)
  awk -v n="$2" '$0 ~ "^[ \t]*" n "\\(\\) \\{" { on = 1 } on { print } on && $0 ~ /^[ \t]*\}[ \t]*$/ { exit } on && $1 == n "()" && $0 ~ /\}[ \t]*$/ { exit }' "$1"
}
# deployments/helm/kind/target-kubeconfig-test.sh helpers (the real functions): a file that is not there has a value, one that cannot be read does not
printf 'a\nb\nc\n' > "$tmp/tk.three"; : > "$tmp/tk.empty"; : > "$tmp/tk.noread"; chmod 000 "$tmp/tk.noread"
tk_cell() { # <label> <want rc> <want out> <command…>
  local label=$1 wrc=$2 wout=$3; shift 3
  { echo ". '$src/scripts/best-effort.sh'"; for fn in yesno fingerprint num_in lines_in; do fn_text "$src/deployments/helm/kind/target-kubeconfig-test.sh" $fn; done; printf '%s\n' "$@"; } > "$tmp/tk.sh"
  trc=0; capture -2 tout trc bash "$tmp/tk.sh" || exit 1
  expect_eq "target-kubeconfig-test ($label): exit" "$wrc" "$trc"
  expect_eq "target-kubeconfig-test ($label): output" "$wout" "$tout"
}
tk_cell "fingerprint: absent file"       0 absent 'fingerprint '"$tmp"'/tk.nothere'
ck_three=$(cksum < "$tmp/tk.three") || exit 1
tk_cell "fingerprint: present file"      0 "$ck_three" 'fingerprint '"$tmp"'/tk.three'
[ "$uid_now" = 0 ] || tk_cell "fingerprint: unreadable file fails (not 'absent')" 1 "" 'fingerprint '"$tmp"'/tk.noread 2>/dev/null'
tk_cell "num_in: absent counter = 0"     0 0 'num_in '"$tmp"'/tk.nothere'
tk_cell "num_in: present counter"        0 $'a\nb\nc' 'num_in '"$tmp"'/tk.three'
[ "$uid_now" = 0 ] || tk_cell "num_in: unreadable counter fails (not 0)" 1 "" 'num_in '"$tmp"'/tk.noread 2>/dev/null'
tk_cell "lines_in: absent file = 0"      0 0 'lines_in '"$tmp"'/tk.nothere'
tk_cell "lines_in: three lines"          0 3 'lines_in '"$tmp"'/tk.three'
tk_cell "lines_in: empty file = 0 (grep 1)" 0 0 'lines_in '"$tmp"'/tk.empty'
tk_cell "lines_in: grep prints 5 then dies (2): a failure, no count" 2 "value_of: command failed (exit 2): grep -c . $tmp/tk.three" 'grep() { echo 5; return 2; }; lines_in '"$tmp"'/tk.three'
tk_cell "yesno: true"                    0 yes 'yesno true'
tk_cell "yesno: false"                   0 no  'yesno false'
tk_cell "yesno: prints then fails → no"  0 $'out\nno' 'f() { echo out; return 3; }; yesno f'
chmod 600 "$tmp/tk.noread"

suite_done=1
echo "scripts/test.sh: $pass passed, $fail failed"
[ "$fail" = 0 ]
