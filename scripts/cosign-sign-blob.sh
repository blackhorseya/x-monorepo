#!/usr/bin/env bash
# .goreleaser.yaml's `signs.cmd` (#215 option C): `cosign sign-blob` with a bounded re-sign on a
# Rekor 409. goreleaser passes the same args as before (sign-blob --bundle=<file> <artifact> --yes)
# and its environment (the ambient OIDC token) reaches cosign untouched.
#
# Why: cosign's 409 handling (v3.1.4, sigstore-go v1.3.0) covers Rekor v1 only. On v2 the 409
# comes back as an error whose upstream remedy is "sign again to generate a different signature"
# (#215). Keyless, so every attempt is a fresh ephemeral key and certificate: a re-sign is a new
# Rekor entry, never a reuse of the one that conflicted.
#
# Fail-closed:
#  - only an output line that is a Rekor conflict (v1 `log/entries][409]`, v2 `adding rekor v2
#    entry: unexpected response: 409`) is retried, at most 2 more times (a
#    constant, not configurable); any other failure exits at once;
#  - a 409 is never success: a failed attempt stays failed, and when the retries run out the
#    script exits non-zero;
#  - the bundle is removed before every attempt and after every failure, and a "successful" cosign
#    that left no bundle, or an empty one, fails (goreleaser would upload a hollow signature).
# Logs say which attempt this is and cosign's own output, nothing from the environment.
#
#   scripts/cosign-sign-blob.sh sign-blob --bundle=<file> <artifact> --yes [--key <key>]
set -euo pipefail

max=3   # 1 sign + at most 2 re-signs
delay=${COSIGN_SIGN_RETRY_DELAY:-5}
case $delay in ''|*[!0-9]*) echo "cosign-sign-blob: COSIGN_SIGN_RETRY_DELAY must be whole seconds, got '$delay'" >&2; exit 2 ;; esac

bundle=
for a in "$@"; do
  case $a in --bundle=*) bundle=${a#--bundle=} ;; esac
done
[ -n "$bundle" ] || { echo "cosign-sign-blob: no --bundle=<file> in the arguments" >&2; exit 2; }

log=$(mktemp); trap 'rm -f "$log"' EXIT
# A conflict is one line that is a Rekor conflict error, not a substring of the whole output (a URL
# or id with 409, another service's 409 next to a line that mentions Rekor, an AlreadyExists from
# something else are ordinary failures). The two shapes of the pinned cosign v3.1.4:
#   v1  `[POST /api/v1/log/entries][409] createLogEntryConflict {...}`  (rekor client)
#   v2  `adding rekor v2 entry: unexpected response: 409 {...}`  (sigstore-go v1.3.0 +
#       rekor-tiles v2.3.0 pkg/client/write, the HTTP writer; scripts/cosign-v2-conflict-smoke.sh
#       runs the real cosign against a loopback v2 server to keep this string honest)
# No gRPC `AlreadyExists` shape: cosign does not print one, so it is not retried.
conflict='log/entries\]\[409\]|adding rekor v2 entry: unexpected response: 409([^0-9]|$)'

attempt=0
while [ "$attempt" -lt "$max" ]; do
  attempt=$((attempt + 1))
  rm -f "$bundle"
  echo "cosign-sign-blob: attempt $attempt/$max"
  rc=0
  cosign "$@" >"$log" 2>&1 || rc=$?
  cat "$log"
  if [ "$rc" = 0 ]; then
    if [ -s "$bundle" ]; then exit 0; fi
    rm -f "$bundle"
    echo "cosign-sign-blob: FAIL: cosign exited 0 but wrote no bundle ($bundle missing or empty)" >&2
    exit 1
  fi
  rm -f "$bundle"
  if ! grep -Eq -- "$conflict" "$log" ; then
    echo "cosign-sign-blob: FAIL: attempt $attempt/$max failed (exit $rc), not a Rekor conflict: not retrying" >&2
    exit "$rc"
  fi
  if [ "$attempt" = "$max" ]; then
    echo "cosign-sign-blob: FAIL: Rekor conflict on all $max attempts" >&2
    exit "$rc"
  fi
  echo "cosign-sign-blob: Rekor conflict, signing again with a fresh key in ${delay}s" >&2
  sleep "$delay"
done
# Not reachable (the last attempt exits above), but a loop that ends is never success.
exit 1
