#!/usr/bin/env bash
# Predicts the version release-drafter will resolve for head: the Conventional Commits types of
# the commits since the last v* tag, bumped the way .github/release-drafter.yml says —
# `!` / BREAKING CHANGE → major, feat → minor, anything else → patch (test is excluded from the
# notes but the resolver still defaults to patch). The drafter itself counts merged PRs since the
# last *published* release and resolves on their titles; on main every commit is a squash-merged PR
# with that title, so the two agree unless someone pushed to main by hand.
#
#   scripts/next-version.sh [base] [head]   # default: last v* tag reachable from HEAD .. HEAD
#   scripts/next-version.sh base head "<subject>"   # + one more commit title: what a PR squash-merged
#                                                   # onto head with that title would resolve to (CI's
#                                                   # release-gate comment uses this with the PR title)
#   task release-gate                       # prints this after the releasable files
#
# Prints "vX.Y.Z (<bump>: <reason>)" and exits 0, or exits 2 when no tag is reachable. Whether a
# release happens at all is scripts/releasable.sh's call, not this one's.
set -euo pipefail
_lib=${BASH_SOURCE[0]%/*}; [ "$_lib" != "${BASH_SOURCE[0]}" ] || _lib=.   # this script's own directory, whatever the cwd (parameter expansion only)
. "$_lib/best-effort.sh" || { echo "next-version: cannot load $_lib/best-effort.sh (capture)" >&2; exit 2; }   # capture: the status of a command next to what it printed
head=${2:-HEAD}
base=${1:-}
if [ -z "$base" ]; then
  capture base describe_rc git describe --tags --abbrev=0 --match 'v*' "$head" 2>/dev/null || exit 1
  [ "$describe_rc" = 0 ] || { base=""; echo "next-version: git describe exited $describe_rc (no v* tag reachable from $head?)" >&2; }
fi
if [ -z "$base" ]; then
  echo "next-version: no previous v* tag reachable from $head" >&2
  exit 2
fi
tag=$(git describe --tags --abbrev=0 --match 'v*' "$base" 2>/dev/null) || { echo "next-version: git describe failed for $base (not a v* tag or a commit after one)" >&2; exit 2; }
IFS=. read -r major minor patch <<<"${tag#v}"
major=${major:-0}; minor=${minor:-0}; patch=${patch%%[-+]*}; patch=${patch:-0}

bump=patch reason="default"
# consider folds one commit (subject + body) into bump / reason; returns 1 once major is settled.
consider() {
  local subject=$1 body=$2 type
  # consider() is called as `consider … || break` / `if consider`: errexit is off in here, so every status is read by hand
  type=$(printf '%s' "$subject" | sed -nE 's/^([a-z]+)(\([^)]*\))?(!)?:.*/\1\3/p') || { echo "next-version: sed over a commit subject failed (exit $?)" >&2; exit 2; }
  case "$type" in
    *!) bump=major reason="breaking: $subject"; return 1 ;;
  esac
  if [ -n "$body" ]; then
    grep_rc=0; grep -q '^BREAKING[ -]CHANGE:' <<<"$body" || grep_rc=$?   # 0 = a footer, 1 = none; 2+ is grep failing, which must not read as "none"
    case $grep_rc in
      0) bump=major reason="breaking: $subject"; return 1 ;;
      1) ;;
      *) echo "next-version: grep over a commit body failed (exit $grep_rc)" >&2; exit 2 ;;
    esac
  fi
  if [ "$type" = feat ] && [ "$bump" = patch ]; then
    bump=minor reason="feat: $subject"
  elif [ "$bump" = patch ] && [ "$reason" = default ] && [ -n "$type" ]; then
    reason="$type: $subject"
  fi
}
# the log first, as a file (it is NUL-separated, which a shell variable cannot hold), then the loop: `done < <(git log …)` would not
# see git dying (#465)
commits=$(mktemp) || { echo "next-version: mktemp failed" >&2; exit 2; }
trap 'rm -f "$commits"' EXIT
log_rc=0; git log -z --first-parent --format='%s%n%b' "$base..$head" > "$commits" || log_rc=$?
[ "$log_rc" = 0 ] || { echo "next-version: git log $base..$head exited $log_rc" >&2; exit 2; }
while IFS= read -r -d '' commit; do
  body=${commit#*$'\n'}
  [ "$body" = "$commit" ] && body=
  consider "${commit%%$'\n'*}" "$body" || break
done < "$commits"
if [ -n "${3:-}" ] && [ "$bump" != major ]; then
  if consider "$3" ""; then :; fi   # consider() returns 1 only to say "major reached, stop scanning"
fi

case "$bump" in
  major) major=$((major + 1)); minor=0; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
echo "v$major.$minor.$patch ($bump — $reason; base $tag)"
