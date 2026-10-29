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
#  - only an `Error: ` output line whose outer status is a Rekor conflict (v1 `log/entries][409]`, v2
#    `adding rekor v2 entry: unexpected response: 409`, the first status on the first `Error: ` line, after
#    the leading `Error: signing <artifact>: `: a 409 quoted in another status's body does not
#    count, #256) is retried, at most 2 more times (a constant,
#    not configurable); any other failure exits at once;
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
# A conflict is the OUTER status of one cosign error line (#256), not any 409 text on a line: a
# non-conflict (HTTP 503, ...) whose body quotes a 409 is an ordinary failure. The two shapes of the
# pinned cosign v3.1.4, each `Error: <context>: <status marker><status> <body>`:
#   v1  `Error: ...[POST /api/v1/log/entries][409] createLogEntryConflict {...}`  (rekor client)
#   v2  `Error: ...adding rekor v2 entry: unexpected response: 409 {...}`  (sigstore-go v1.3.0 +
#       rekor-tiles v2.3.0 pkg/client/write, the HTTP writer; scripts/cosign-v2-conflict-smoke.sh
#       runs the real cosign against a loopback v2 server to keep this string honest)
# Only the FIRST `Error: ` line of the output is the outer error: cosign prints its error once and a
# multi-line response body follows it, so a later line that looks like `Error: ...409` is body text.
# On that line cosign's own context comes first: `Error: signing <artifact>: <what failed>: <status
# marker><status> <body>`. The artifact is an argument of this script (identified below) and can hold any text (a status
# marker, `entry`, `503 `), so exactly that prefix is cut off, anchored at the start of the line and
# not by deleting the text everywhere. A line without that prefix (or an artifact that could not be
# identified) is not classified at all: fail closed. Then the FIRST (leftmost) status marker must carry exactly
# 409 (a quoted marker in the body comes after the real one). Matching is per line, so lines of a
# mixed log never combine.
# No gRPC `AlreadyExists` shape: cosign does not print one, so it is not retried.
status_re='(log/entries\]\[([0-9]+)\]|adding rekor v2 entry: unexpected response: ([0-9]+)([^0-9]|$))'
# The artifact is the first positional argument after the subcommand (`sign-blob --bundle=<file>
# <artifact> --yes ...`, goreleaser's order), skipping flags and the value of the flags that take a
# separate one. The subcommand and every other argument are NOT candidates: a file named
# `sign-blob: <marker>` must not be taken for the prefix.
artifact=
skip=0
for a in "${@:2}"; do
  if [ "$skip" = 1 ]; then skip=0; continue; fi
  case $a in
    --key|--bundle|--signing-config|--output-signature|--output-certificate|--certificate) skip=1 ;;
    -*) ;;
    *) artifact=$a; break ;;
  esac
done
is_conflict() { # is_conflict <file>: the first `Error: ` line of it is a Rekor conflict error
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    case $line in 'Error: '*) ;; *) continue ;; esac
    # Fail closed: without the exact `Error: signing <artifact>: ` context nothing is classified (an
    # unknown value flag before the artifact, or an unexpected error shape, is a plain failure).
    [ -n "$artifact" ] || return 1
    case $line in "Error: signing $artifact: "*) line=${line#"Error: signing $artifact: "} ;; *) return 1 ;; esac
    [[ $line =~ $status_re ]] || return 1
    [ "${BASH_REMATCH[2]}${BASH_REMATCH[3]}" = 409 ]
    return
  done <"$1"
  return 1
}

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
  if ! is_conflict "$log"; then
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
