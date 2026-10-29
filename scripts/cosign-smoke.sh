#!/usr/bin/env bash
# Runs .goreleaser.yaml's `signs` command exactly as goreleaser will, against a throwaway file and
# key instead of the keyless OIDC flow, so a flag the installed cosign no longer honours fails a
# PR and not the release (#119: cosign v3 ignores --output-signature and wants --bundle). The
# release job and ci.yml's release-snapshot job install the same cosign-installer version — keep
# them equal or this proves nothing. Locally: scripts/cosign-smoke.sh (needs cosign, openssl,
# python3).
#
# Second case (#195): the same artifact signed twice with the same key. RSA PKCS#1 v1.5 signatures
# are deterministic, so the second Rekor POST is byte-identical to the first and Rekor answers 409
# "equivalent entry already exists" — what v0.36.0's first release attempt hit when the cosign
# client re-sent a POST Rekor had already committed. cosign v3.0.6 (sigstore-go v1.1.4) fails the
# whole sign on it; v3.1.4 (sigstore-go v1.3.0, PR #647) fetches the existing entry and the
# bundle still verifies. Hits the public Rekor, like the first case.
set -euo pipefail
cd "$(dirname "$0")/.."
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT

# signs_cmd <artifact>: the signs entry — cmd, args and the signature template, with goreleaser's
# ${artifact} / ${signature} filled in — as $cmd (the command line) and $sig (the bundle it writes).
# `--yes` is kept; the caller appends `--key`, which turns the keyless flow into a local one.
signs_cmd() {
  cmd=()
  while IFS= read -r line; do cmd+=("$line"); done < <(python3 - "$1" <<'PY'
import sys, yaml
art = sys.argv[1]
s = yaml.safe_load(open(".goreleaser.yaml"))["signs"][0]
sig = s.get("signature", "${artifact}.sig").replace("${artifact}", art)
sub = lambda a: a.replace("${artifact}", art).replace("${signature}", sig)
print(s.get("cmd", "cosign")); print(*map(sub, s["args"]), sep="\n"); print(sig)
PY
)
  sig=${cmd[${#cmd[@]}-1]}; unset "cmd[${#cmd[@]}-1]"
}

echo smoke > "$work/checksums.txt"
signs_cmd "$work/checksums.txt"
( cd "$work" && COSIGN_PASSWORD= cosign generate-key-pair >/dev/null 2>&1 )
echo "+ ${cmd[*]} --key $work/cosign.key"
COSIGN_PASSWORD= "${cmd[@]}" --key "$work/cosign.key"
[ -s "$sig" ] || { echo "signature file $sig was not written"; exit 1; }
# Verify the way the release notes tell users to (minus identity flags, plus the key).
if [[ $sig == *.sigstore.json ]]; then
  cosign verify-blob --key "$work/cosign.pub" --bundle "$sig" "$work/checksums.txt"
else
  cosign verify-blob --key "$work/cosign.pub" --signature "$sig" "$work/checksums.txt"
fi
echo "cosign $(cosign version 2>&1 | sed -n 's/^GitVersion: *//p') signs and verifies with .goreleaser.yaml's flags"

# Same artifact, same deterministic key, signed twice (#195). The content is unique per run, so
# the first POST is always a new entry and the second one the equivalent entry.
openssl genrsa -out "$work/rsa.pem" 2048 2>/dev/null
( cd "$work" && COSIGN_PASSWORD= cosign import-key-pair --key rsa.pem >/dev/null 2>&1 )
echo "equivalent-entry $(openssl rand -hex 16)" > "$work/equiv.txt"
signs_cmd "$work/equiv.txt"
COSIGN_PASSWORD= "${cmd[@]}" --key "$work/import-cosign.key" >/dev/null \
  || { echo "FAIL: first signature of the artifact failed (Rekor unreachable, not the 409 case)"; exit 1; }
# Both signs write the same $sig: drop the first bundle so the checks below see the second one only.
rm -f "$sig"
if ! out=$(COSIGN_PASSWORD= "${cmd[@]}" --key "$work/import-cosign.key" 2>&1); then
  echo "FAIL: signing the same artifact again met an equivalent Rekor entry and cosign gave up (#195):"
  printf '%s\n' "$out" | sed -E 's/[0-9a-f]{40,}/<id>/g' | tail -n 2
  exit 1
fi
[ -s "$sig" ] || { echo "FAIL: the second signature wrote no bundle ($sig missing or empty)"; exit 1; }
cosign verify-blob --key "$work/import-cosign.pub" --bundle "$sig" "$work/equiv.txt" \
  || { echo "FAIL: the equivalent-entry bundle does not verify (signature or log inclusion)"; exit 1; }
echo "signing the same artifact twice reuses the existing Rekor entry and the bundle verifies"
