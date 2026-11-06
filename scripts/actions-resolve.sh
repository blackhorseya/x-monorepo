#!/usr/bin/env bash
# Every `uses: owner/repo[/path]@ref` in .github must resolve on GitHub — a tag, a branch or a
# commit. actionlint checks the YAML, not the remote: `sigstore/cosign-installer@v4` passed every
# PR check and broke the first release on main (#115), because release.yml only ever runs there.
# ci.yml's actions-resolve job runs this on every PR; locally: scripts/actions-resolve.sh (needs gh).
set -euo pipefail
self_dir=${0%/*}; [ "$self_dir" != "$0" ] || self_dir=.   # dirname without a $(…)
cd "$self_dir/.." || exit 1
fail=0
# the list first, then the loop: `done < <(…)` would not see grep / sed / sort dying (#465); no `uses:` at all is not a pass
rc=0; uses=$(grep -rhE '^[[:space:]]*-?[[:space:]]*uses:[[:space:]]*[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@' .github \
  | sed -E 's/^[[:space:]]*-?[[:space:]]*uses:[[:space:]]*//; s/[[:space:]#].*$//' | sort -u) || rc=$?   # the YAML key only, not prose mentioning `uses:`
[ "$rc" = 0 ] || { echo "actions-resolve: listing the uses: lines exited $rc" >&2; exit 2; }
[ -n "$uses" ] || { echo "actions-resolve: no uses: line found under .github" >&2; exit 2; }
while IFS= read -r use; do
  repo=${use%%@*}; ref=${use##*@}
  repo=$(printf '%s' "$repo" | cut -d/ -f1,2)   # owner/repo/sub/path → owner/repo
  if gh api "repos/$repo/git/ref/tags/$ref" --silent 2>/dev/null \
    || gh api "repos/$repo/git/ref/heads/$ref" --silent 2>/dev/null \
    || { [[ $ref =~ ^[0-9a-f]{40}$ ]] && gh api "repos/$repo/git/commits/$ref" --silent 2>/dev/null; }; then
    echo "ok    $use"
  else
    echo "MISSING $use (no tag, branch or commit named $ref in $repo)"; fail=1
  fi
done <<<"$uses"
exit $fail
