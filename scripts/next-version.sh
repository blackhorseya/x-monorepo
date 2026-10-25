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
head=${2:-HEAD}
base=${1:-$(git describe --tags --abbrev=0 --match 'v*' "$head" 2>/dev/null || true)}
if [ -z "$base" ]; then
  echo "next-version: no previous v* tag reachable from $head" >&2
  exit 2
fi
tag=$(git describe --tags --abbrev=0 --match 'v*' "$base" 2>/dev/null || echo "$base")
IFS=. read -r major minor patch <<<"${tag#v}"
major=${major:-0}; minor=${minor:-0}; patch=${patch%%[-+]*}; patch=${patch:-0}

bump=patch reason="default"
# consider folds one commit (subject + body) into bump / reason; returns 1 once major is settled.
consider() {
  local subject=$1 body=$2 type
  type=$(printf '%s' "$subject" | sed -nE 's/^([a-z]+)(\([^)]*\))?(!)?:.*/\1\3/p')
  case "$type" in
    *!) bump=major reason="breaking: $subject"; return 1 ;;
  esac
  if [ -n "$body" ] && printf '%s' "$body" | grep -q '^BREAKING[ -]CHANGE:'; then
    bump=major reason="breaking: $subject"; return 1
  fi
  if [ "$type" = feat ] && [ "$bump" = patch ]; then
    bump=minor reason="feat: $subject"
  elif [ "$bump" = patch ] && [ "$reason" = default ] && [ -n "$type" ]; then
    reason="$type: $subject"
  fi
}
while IFS= read -r -d '' commit; do
  body=${commit#*$'\n'}
  [ "$body" = "$commit" ] && body=
  consider "${commit%%$'\n'*}" "$body" || break
done < <(git log -z --first-parent --format='%s%n%b' "$base..$head")
if [ -n "${3:-}" ] && [ "$bump" != major ]; then
  consider "$3" "" || true
fi

case "$bump" in
  major) major=$((major + 1)); minor=0; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
echo "v$major.$minor.$patch ($bump — $reason; base $tag)"
