#!/usr/bin/env bash
# Binds the two dev Dex accounts (deployments/docker/dex/config.yaml) to identity accounts through the
# real invite flow (decision d-20261030034939389099-45, #274 part 2): for each email, find or create
# the account (admin API key) → CreateInvite (admin key) → log in at Dex (scripts/dex-token.sh) →
# ClaimInvite (that bearer token). Only RPCs: no direct write to external_identities, no new CLI.
#
#   ADMIN_API_KEY=<admin key> scripts/dex-prebind.sh      # task dex-prebind
#
# admin@example.com binds to an account of its own named admin@example.com with the admin role
# (PREBIND_ADMIN_ROLE, default admin = auth.admin_role); user@example.com to one with no role. They
# are separate from `task bootstrap`'s root, which stays the API-key break-glass account.
#
# Rerunnable: an account is found by name; an email already bound for ISSUER (ListAccountIdentities)
# is skipped without asking Dex for a token; a pending invite is reused, never duplicated.
# Dev only: refuses any ISSUER but the dev Dex, and any BASE_URL that is not a local server (the
# admin key goes there). Nothing secret is printed: the key and the tokens only travel on curl's
# stdin config (not argv), and a failed RPC prints its status and Connect / x-error-code, never the body.
#
# ADMIN_API_KEY (required), BASE_URL (default http://127.0.0.1:8080), ISSUER, DEX_TOKEN_CMD (default
# dex-token.sh next to this script), PREBIND_ADMIN_ROLE. Needs curl and jq.
set -euo pipefail
dev_issuer=http://dex.localtest.me:5556/dex
issuer=${ISSUER:-$dev_issuer}
base=${BASE_URL:-http://127.0.0.1:8080}
role=${PREBIND_ADMIN_ROLE:-admin}
token_cmd=${DEX_TOKEN_CMD:-$(dirname "$0")/dex-token.sh}
svc=identity.v1.IdentityService
die() { echo "dex-prebind: $*" >&2; exit 1; }

[ "$issuer" = "$dev_issuer" ] || die "the issuer must be the dev Dex ($dev_issuer); this only runs against the dev IdP"
case $base in
  http://localhost|http://localhost:*|http://localhost/*|http://127.0.0.1|http://127.0.0.1:*|http://127.0.0.1/*|http://\[::1\]|http://\[::1\]:*|http://\[::1\]/*) ;;
  *) die "BASE_URL must be a local http server (localhost, 127.0.0.1, [::1]); the admin key is sent there" ;;
esac
[ -n "${ADMIN_API_KEY:-}" ] || die "ADMIN_API_KEY is required (an admin API key: task bootstrap's, or a break-glass auth.api_keys entry)"
command -v jq >/dev/null || die "jq is required"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# rpc <Method> <json> <header-name> <header-value>: POSTs to the Connect endpoint and leaves the
# response in $resp. The credential header goes through curl's stdin config, so it never shows in a
# process list. On failure only the HTTP status, Connect code and x-error-code are reported.
resp=
rpc() {
  local method=$1 body=$2 name=$3 value=$4 status
  value=${value//\\/\\\\}; value=${value//\"/\\\"}
  status=$(printf 'header = "%s: %s"\n' "$name" "$value" | curl -sS -K - -o "$work/body" -D "$work/headers" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data-binary "$body" "$base/$svc/$method") || die "$method: cannot reach $base"
  if [ "$status" != 200 ]; then
    local code xcode
    code=$(jq -r '.code // empty' "$work/body" 2>/dev/null || true)
    xcode=$(tr -d '\r' < "$work/headers" | sed -n 's/^[Xx]-[Ee]rror-[Cc]ode: *//p' | head -1)
    die "$method failed: http $status, code ${code:-?}, x-error-code ${xcode:-?}"
  fi
  resp=$(cat "$work/body")
}
admin() { rpc "$1" "$2" X-API-Key "$ADMIN_API_KEY"; }

# find_account <name>: the id of the account with that name, or empty (pages through ListAccounts).
find_account() {
  local token='' id
  while :; do
    admin ListAccounts "$(jq -nc --arg t "$token" '{pageSize: 100, pageToken: $t}')"
    id=$(jq -r --arg n "$1" '[.accounts[]? | select(.name == $n) | .id][0] // empty' <<<"$resp")
    if [ -n "$id" ]; then echo "$id"; return; fi
    token=$(jq -r '.nextPageToken // empty' <<<"$resp")
    [ -n "$token" ] || return 0
  done
}

# prebind <email> <role...>: leaves the email's Dex identity bound to its account.
prebind() {
  local email=$1; shift
  local roles id
  roles=$(jq -nc '$ARGS.positional' --args "$@")
  id=$(find_account "$email")
  if [ -z "$id" ]; then
    admin CreateAccount "$(jq -nc --arg n "$email" --argjson r "$roles" '{name: $n, roles: $r}')"
    id=$(jq -er '.account.id' <<<"$resp") || die "CreateAccount: no account id in the response"
    echo "created account for $email"
  fi
  admin ListAccountIdentities "$(jq -nc --arg a "$id" '{accountId: $a}')"
  if jq -e --arg i "$issuer" '[.identities[]? | select(.issuer == $i)] | length > 0' <<<"$resp" >/dev/null; then
    echo "already bound: $email"
    return
  fi
  if jq -e --arg e "$email" '[.invites[]? | select(.email == $e and .status == "INVITE_STATUS_PENDING")] | length > 0' <<<"$resp" >/dev/null; then
    echo "pending invite exists: $email"
  else
    admin CreateInvite "$(jq -nc --arg a "$id" --arg e "$email" '{accountId: $a, email: $e}')"
    echo "invited $email"
  fi
  local token
  token=$(ISSUER=$issuer "$token_cmd" "$email") || die "could not get a Dex token for $email (is Dex up and dex.localtest.me resolvable? docs/operations.md)"
  [ -n "$token" ] || die "empty Dex token for $email"
  rpc ClaimInvite '{}' Authorization "Bearer $token"
  echo "bound $email"
}

prebind admin@example.com "$role"
prebind user@example.com
