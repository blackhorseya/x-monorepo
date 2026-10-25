#!/usr/bin/env bash
# Verifies a published release the way its notes tell a reader to (.github/release-drafter.yml's
# Verify block): the checksums.txt cosign bundle, the archives against checksums.txt, the
# linux_amd64 archive's GitHub artifact attestation, and the keyless signature on each of the four
# images. release.yml's verify job runs it right after publish — a release whose own instructions
# fail is a red run, not something found by hand a day later.
#
#   scripts/release-verify.sh vX.Y.Z    # needs gh (logged in), cosign, sha256sum; ghcr login for the
#                                        # images (the packages are private): docker/podman login
set -euo pipefail
tag=${1:?usage: scripts/release-verify.sh vX.Y.Z}
repo=${REPO:-blackhorseya/x-monorepo}
version=${tag#v}
id="https://github.com/$repo/.github/workflows/release.yml@refs/heads/main"
iss=https://token.actions.githubusercontent.com

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
cd "$dir"
gh release download "$tag" --repo "$repo" -p 'checksums.txt*' -p '*.tar.gz'
echo "== archives match checksums.txt"
sha256sum -c --ignore-missing checksums.txt
echo "== checksums.txt cosign bundle"
cosign verify-blob --certificate-identity "$id" --certificate-oidc-issuer "$iss" \
  --bundle checksums.txt.sigstore.json checksums.txt
echo "== archive attestation"
gh attestation verify "x-monorepo_${version}_linux_amd64.tar.gz" --repo "$repo"
for app in rpc worker migrate console; do
  echo "== image signature: $app:$version"
  cosign verify --certificate-identity "$id" --certificate-oidc-issuer "$iss" \
    "ghcr.io/$repo/$app:$version" --output text >/dev/null
  echo "ok"
done
echo "release-verify: $tag OK"
