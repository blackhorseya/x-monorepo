#!/usr/bin/env bash
# Logs in to the dev Dex (deployments/docker/dex/config.yaml) the way the Console will — authorization
# code + PKCE, no password grant — and prints the access token (a JWT) or, with --id-token, the
# ID token, on stdout. Dev tool: it drives Dex's login form with curl, so it only fits the static
# password connector.
#
#   scripts/dex-token.sh [admin@example.com|user@example.com] [--id-token]
#   curl -H "Authorization: Bearer $(scripts/dex-token.sh)" http://localhost:8080/api/...
#
# ISSUER / CLIENT_ID / REDIRECT_URI / DEX_PASSWORD override the defaults. The issuer's host must
# resolve (public DNS for *.localtest.me, or /etc/hosts offline: docs/operations.md).
set -euo pipefail
email=admin@example.com
want=access_token
for a in "$@"; do
  case $a in
    --id-token) want=id_token ;;
    *@*) email=$a ;;
    *) echo "usage: $0 [email] [--id-token]" >&2; exit 2 ;;
  esac
done
issuer=${ISSUER:-http://dex.localtest.me:5556/dex}
client=${CLIENT_ID:-x-console}
redirect=${REDIRECT_URI:-http://localhost:5173/auth/callback}
password=${DEX_PASSWORD:-password}

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
verifier=$(openssl rand 48 | b64url)
challenge=$(printf %s "$verifier" | openssl dgst -sha256 -binary | b64url)
state=$(openssl rand -hex 8)

jar=$(mktemp)
trap 'rm -f "$jar"' EXIT
urlenc() { jq -rn --arg v "$1" '$v|@uri'; }

# 1. /auth → (with one connector Dex redirects straight to its login form); -L ends on the form.
auth="$issuer/auth?client_id=$client&redirect_uri=$(urlenc "$redirect")&response_type=code&scope=$(urlenc 'openid email profile')&state=$state&code_challenge=$challenge&code_challenge_method=S256"
form=$(curl -fsS -L -c "$jar" -b "$jar" -o /dev/null -w '%{url_effective}' "$auth")

# 2. POST the credentials; skipApprovalScreen makes the 303 land on redirect_uri?code=…
location=$(curl -sS -c "$jar" -b "$jar" -o /dev/null -w '%{redirect_url}' \
  --data-urlencode "login=$email" --data-urlencode "password=$password" "$form")
case $location in
  "$redirect"*) ;;
  *"/approval"*) location=$(curl -fsS -L -c "$jar" -b "$jar" -o /dev/null -w '%{url_effective}' "$location" || true) ;;
esac
case $location in
  "$redirect"*) ;;
  *) echo "login did not reach redirect_uri (wrong password? client/redirect not in dex config?)" >&2; exit 1 ;;
esac
case $location in *"state=$state"*) ;; *) echo "state mismatch" >&2; exit 1 ;; esac
code=$(printf %s "$location" | sed -n 's/.*[?&]code=\([^&]*\).*/\1/p')
[ -n "$code" ] || { echo "no code in redirect" >&2; exit 1; }

# 3. Exchange the code with the PKCE verifier.
curl -fsS "$issuer/token" \
  --data-urlencode grant_type=authorization_code --data-urlencode "code=$code" \
  --data-urlencode "redirect_uri=$redirect" --data-urlencode "client_id=$client" \
  --data-urlencode "code_verifier=$verifier" | jq -er ".$want"
