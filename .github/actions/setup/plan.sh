#!/usr/bin/env bash
# The one place that decides who may WRITE a cache and which Go build cache a job uses (#450).
# Called by .github/actions/setup and .github/actions/playwright; every cache step reads its
# outputs, none repeats the condition. scripts/test.sh runs it against a table of triggers and
# of ci.yml's own job names.
#
#   EVENT_NAME=push REF=refs/heads/main DEFAULT_BRANCH=main SCOPE=lint .github/actions/setup/plan.sh
#
# Inputs (environment): EVENT_NAME github.event_name, REF github.ref, DEFAULT_BRANCH
# github.event.repository.default_branch, SCOPE the setup action's cache-scope.
# Outputs (to $GITHUB_OUTPUT, else stdout), one key=value per line:
#   write       "true" only for push / workflow_dispatch on the default branch. GitHub gives every
#               other trigger that resolves to the default branch (workflow_run, pull_request_target,
#               schedule-less issue_comment, ...) read-only access to its caches, and a pull request
#               writes into refs/pull/N/merge, where it only evicts the entries main needs.
#   week        ISO week, the generation of the Go build caches.
#   group       which shared Go build cache the scope reads ("" = none: module cache only).
#   save_build  "true" when this run writes the group's build cache: write, and the scope is the
#               one designated job per group (its compile set is the superset of the group's).
set -euo pipefail

event=${EVENT_NAME:-} ref=${REF:-} default=${DEFAULT_BRANCH:-} scope=${SCOPE:-}

write=false
if [[ -n $default && $ref == "refs/heads/$default" && ( $event == push || $event == workflow_dispatch ) ]]; then
  write=true
fi

# scope -> group, and whether this scope is the group's designated saver. A scope that is not
# listed gets no build cache at all (it can never grow a new entry by accident); scripts/test.sh
# fails when a job in ci.yml that calls the setup action is missing here.
group="" saver=false
case $scope in
  test-apps/rpc)                                          group=race  saver=true ;;
  test-*)                                                 group=race ;;
  lint)                                                   group=check saver=true ;;
  gen-freshness|work-sync|coverage|scripts-test|affected) group=check ;;
  build)                                                  group=bin   saver=true ;;
  k6|k6-auth|k6-wire|console-e2e|console-image|helm)      group=bin ;;
  helm-kind)                                              group=kind  saver=true ;;
  helm-kind-keda)                                         group=kind ;;
  release-snapshot)                                       group=cross saver=true ;;
esac

save_build=false
if [[ $write == true && $saver == true ]]; then save_build=true; fi

# an assignment, not `echo "week=$(date ...)"`: a failing date must stop the script, not become "week="
week=$(date -u +%G-W%V)
if [[ ! $week =~ ^[0-9]{4}-W[0-9]{2}$ ]]; then
  echo "plan: date did not give an ISO week" >&2
  exit 1
fi

out=${GITHUB_OUTPUT:-/dev/stdout}
{
  echo "write=$write"
  echo "week=$week"
  echo "group=$group"
  echo "save_build=$save_build"
} >> "$out"
