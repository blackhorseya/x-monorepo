#!/usr/bin/env bash
# Runs .goreleaser.yaml's `signs` command exactly as goreleaser will, against a throwaway file and
# key instead of the keyless OIDC flow, so a flag the installed cosign no longer honours fails a
# PR and not the release (#119: cosign v3 ignores --output-signature and wants --bundle). The
# release job and ci.yml's release-snapshot job install the same cosign-installer version — keep
# them equal or this proves nothing. Locally: scripts/cosign-smoke.sh (needs cosign, python3).
set -euo pipefail
cd "$(dirname "$0")/.."
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
echo smoke > "$work/checksums.txt"
# The signs entry: cmd, args and the signature template, with goreleaser's ${artifact} /
# ${signature} filled in. `--yes` is kept; `--key` turns the keyless flow into a local one.
cmd=()
while IFS= read -r line; do cmd+=("$line"); done < <(python3 - "$work/checksums.txt" <<'PY'
import sys, yaml
art = sys.argv[1]
s = yaml.safe_load(open(".goreleaser.yaml"))["signs"][0]
sig = s.get("signature", "${artifact}.sig").replace("${artifact}", art)
sub = lambda a: a.replace("${artifact}", art).replace("${signature}", sig)
print(s.get("cmd", "cosign")); print(*map(sub, s["args"]), sep="\n"); print(sig)
PY
)
sig=${cmd[${#cmd[@]}-1]}; unset "cmd[${#cmd[@]}-1]"
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
