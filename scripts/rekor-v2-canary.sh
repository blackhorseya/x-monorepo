#!/usr/bin/env bash
# Fails when the default signing config of the installed cosign lists a Rekor log that is not v1
# (#215). cosign-smoke.sh's equivalent-entry case, and release.yml's cosign-release v3.1.4 (#204,
# sigstore-go v1.3.0 fetches the entry Rekor already holds), only cover Rekor v1: on a v2 log
# sigstore-go returns the 409 as an error ("A 409 Conflict from Rekor v2 is currently not
# handled"), so the first release after sigstore's TUF root flips the default fails on the 409 the
# smoke no longer reproduces. This turns that into a red CI run beforehand.
#
#   scripts/rekor-v2-canary.sh    # needs cosign (the one release.yml installs), jq, network
#   REKOR_CANARY_SIGNING_CONFIG=<file> scripts/rekor-v2-canary.sh   # judge that file, no cosign
#
# What is read: the signing config cosign signs with — `cosign initialize` into a fresh TUF_ROOT
# fetches the TUF-verified targets, among them signing_config*.json — and its rekorTlogUrls. Not
# the URL strings: every entry must say majorApiVersion 1, so a v2 log and a version this script
# has never heard of both fail. Not trusted_root.json either: its `tlogs` are verification keys and
# may list a v2 log long before anything signs with it. rekorTlogConfig.selector is not modelled:
# one non-v1 log anywhere in the list is enough, ANY may pick it. Nor validFor: an entry that is
# valid later becomes the default without a repo commit, which is the case to catch.
#
# A fresh TUF_ROOT, not ~/.sigstore/root: a cached copy would answer with what the runner saw last.
#
# Fail-closed when TUF or the config cannot be read (retried 3 times first). A skip would be green
# exactly when this cannot see, and cosign-smoke.sh next to it fails the same way when Rekor is
# unreachable; the TUF failure carries its own message so a flake reads differently from a v2.
set -euo pipefail

issue='#215, https://github.com/blackhorseya/x-monorepo/issues/215'
cfg=${REKOR_CANARY_SIGNING_CONFIG:-}

if [ -z "$cfg" ]; then
  command -v cosign >/dev/null || { echo "FAIL: cosign not found: install the cosign release.yml pins (cosign-installer, cosign-release)"; exit 1; }
  tuf=$(mktemp -d); trap 'rm -rf "$tuf"' EXIT
  ok=
  for attempt in 1 2 3; do
    if err=$(TUF_ROOT="$tuf" cosign initialize 2>&1); then ok=1; break; fi
    echo "cosign initialize failed (attempt $attempt/3): $(printf '%s' "$err" | tail -n 1)" >&2
    [ "$attempt" = 3 ] || sleep "${REKOR_CANARY_RETRY_DELAY:-5}"
  done
  [ -n "$ok" ] || { echo "FAIL: cannot resolve the default signing config: cosign initialize (TUF) failed 3 times, nothing was checked. A network or sigstore outage, not a Rekor v2 finding; re-run."; exit 1; }
  found=$(find "$tuf" -path '*/targets/signing_config*.json' -type f)
  [ "$(printf '%s\n' "$found" | grep -c .)" = 1 ] || {
    echo "FAIL: expected one signing_config*.json among cosign's TUF targets, found:"; echo "${found:-  (none)}"
    echo "cosign's TUF layout or the signing config's file name changed; fix this script before trusting it ($issue)."; exit 1
  }
  cfg=$found
fi

[ -f "$cfg" ] || { echo "FAIL: signing config $cfg does not exist"; exit 1; }
logs=$(jq -c '[.rekorTlogUrls[]?]' "$cfg" 2>/dev/null) || { echo "FAIL: $cfg is not a signing config jq can read"; exit 1; }
[ "$logs" != "[]" ] || { echo "FAIL: the signing config lists no Rekor log (rekorTlogUrls empty or missing): cannot tell which Rekor cosign would sign against"; exit 1; }

version=$(cosign version 2>/dev/null | sed -n 's/^GitVersion: *//p' || true)
bad=$(jq -r '.[] | select(.majorApiVersion != 1) | "  \(.url)  majorApiVersion=\(.majorApiVersion)  validFor=\(.validFor | tojson)  operator=\(.operator)"' <<<"$logs")
if [ -n "$bad" ]; then
  cat <<MSG
FAIL: cosign ${version:-?}'s default signing config lists a Rekor log that is not v1:
$bad
release.yml's cosign v3.1.4 fetches the entry behind a Rekor 409 (#204) for v1 only; a v2 log fails
the whole sign on a 409 instead. Do not release on this cosign until one of these is done ($issue):
  - sign-blob / sign get a bounded re-sign wrapper for the 409 (issue option C), proven by
    scripts/cosign-smoke.sh against the v2 log; or
  - cosign-release is pinned to a version whose default signing config is still v1-only (the pins
    in release.yml and ci.yml move together; rekor-canary.yml follows release.yml); or
  - cosign signs with an explicit v1 signing config.
MSG
  exit 1
fi
echo "ok: every Rekor log in the default signing config is v1 (cosign ${version:-?}): $(jq -r 'map(.url) | join(", ")' <<<"$logs")"
