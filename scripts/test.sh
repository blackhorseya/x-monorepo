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
expect_eq "another workflow is actions-resolve only" "actions_resolve" "$(jobs_on wf .github/workflows/labeler.yml)"
# release.yml pins cosign-release (#215): a bump there must run release-snapshot's cosign smoke and
# Rekor canary, which ci-changes.sh used to skip (release.yml only set the actions area).
expect_eq "release.yml runs release-snapshot too" "release_snapshot actions_resolve" "$(jobs_on relyml .github/workflows/release.yml)"
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
  echo "$rc $(printf '%s' "$out" | grep -m1 '^FAIL' || true)"
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
sign_blob() { # SB_ART=<file name> overrides the artifact name
  local d="$tmp/sb-$1" art="${SB_ART:-checksums.txt}"; mkdir -p "$d"; echo artifact > "$d/$art"
  : > "$d/args"; rm -f "$d/count"
  sb_bundle="$d/checksums.txt.sigstore.json"
  set +e
  if [ -n "${SB_REL:-}" ]; then # a bare relative artifact name, as cosign prints it
    sb_out=$(cd "$d" && PATH="$signfake:$PATH" FAKE_COUNT="$d/count" FAKE_ARGS="$d/args" FAKE_SEQ="$2" SIGSTORE_ID_TOKEN=secret-oidc-token FAKE_ART="$art" \
      COSIGN_SIGN_RETRY_DELAY=0 "$src/scripts/cosign-sign-blob.sh" sign-blob "--bundle=$sb_bundle" ${SB_PRE:-} "$art" --yes 2>&1)
  else
    sb_out=$(cd "$src" && PATH="$signfake:$PATH" FAKE_COUNT="$d/count" FAKE_ARGS="$d/args" FAKE_SEQ="$2" SIGSTORE_ID_TOKEN=secret-oidc-token \
      COSIGN_SIGN_RETRY_DELAY=0 env ${SB_EXTRA:-} scripts/cosign-sign-blob.sh sign-blob "--bundle=$sb_bundle" "$d/$art" --yes 2>&1)
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
# A value flag the wrapper does not know, before the artifact: the artifact cannot be identified, so
# nothing is classified (fail closed: one attempt), whatever the file name says (PR #259 review).
odd409='adding rekor v2 entry: unexpected response: 409 artifact'
SB_REL=1 SB_PRE='--oidc-client-id fixture-client' SB_ART=$odd409 sign_blob unknownflag-503 "v2path503 v2path503 v2path503"
expect_eq    "sign: an unrecognised artifact never turns a 503 into a conflict" 1 "$sb_tries"
SB_REL=1 SB_PRE='--oidc-client-id fixture-client' sign_blob unknownflag-409 "v2path409 ok"
expect_eq    "sign: an unrecognised artifact fails closed even on a real 409" 1 "$sb_tries"
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
long_log=$(printf '=== RUN   TestX\n    x_test.go:10: target assertion\n'; for _ in $(seq 1 20000); do printf '    long diagnostic line %0100d\n' 0; done; printf -- '--- FAIL: TestX (0.00s)\nFAIL\n')
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

echo "scripts/test.sh: $pass passed, $fail failed"
[ "$fail" = 0 ]
