#!/usr/bin/env bash
# Writes (markdown, stdout) what merging a PR does to the release train — the sticky comment
# .github/workflows/release-gate.yml keeps on every PR:
#
#   scripts/release-gate-pr.sh <main> <head> "<pr title>"
#
# main is the branch the PR targets (origin/main in CI), head the PR's merge commit (what
# actions/checkout gives a pull_request run: main + the PR). Two things are answered, the way
# release.yml's prepare job will answer them after a squash merge:
#   - does anything in this PR reach an artifact (scripts/releasable.sh main..head), and
#   - what version does main resolve to afterwards: the releasable changes already waiting on
#     main since the last tag plus this PR's, bumped from the pending commit titles plus this
#     PR's title (scripts/next-version.sh — the squash commit carries the PR title).
# Exit 0 always; the comment is advice, never a gate.
set -euo pipefail
main=$1 head=$2 title=$3
repo_url=${REPO_URL:-https://github.com/blackhorseya/x-monorepo}

base=$(git describe --tags --abbrev=0 --match 'v*' "$main" 2>/dev/null || true)
echo '<!-- release-gate -->'
echo '### Release gate'
echo
if [ -z "$base" ]; then
  echo "No \`v*\` tag is reachable from \`$main\`: the first merge releases unconditionally."
  exit 0
fi
own=$(scripts/releasable.sh "$main" "$head")
pending=$(scripts/releasable.sh "$base" "$main")
if [ -z "$own$pending" ]; then
  echo "Merging **cuts no release**: nothing in this PR or waiting on main since [\`$base\`]($repo_url/releases/tag/$base) reaches an artifact."
else
  next=$(scripts/next-version.sh "$base" "$main" "$title")
  echo "Merging **releases \`${next%% *}\`** ${next#* }."
fi
echo
if [ -n "$own" ]; then
  echo "- Releasable files in this PR: $(printf '%s\n' "$own" | sed 's/.*/`&`/' | paste -sd, - | sed 's/,/, /g')"
else
  echo "- Releasable files in this PR: none (docs / CI / tests / chart only — see \`scripts/releasable.sh\`)"
fi
if [ -n "$pending" ]; then
  n=$(printf '%s\n' "$pending" | wc -l | tr -d ' ')
  echo "- Already waiting on main since \`$base\`: $n releasable file(s), so a release is due at the next merge either way."
else
  echo "- Already waiting on main since \`$base\`: nothing."
fi
echo
echo "<sub>Same scripts as \`task release-gate\` (\`scripts/releasable.sh\`, \`scripts/next-version.sh\`); the squash commit carries this PR's title. Refreshed on push and retitle.</sub>"
