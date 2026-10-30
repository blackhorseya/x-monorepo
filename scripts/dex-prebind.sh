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
# BASE_URL is matched whole, not by prefix: a prefix test lets "http://localhost:x@evil.example" through
# (curl reads that as userinfo and connects to evil.example). Only scheme + an exact local host +
# an optional port in 1-65535 (+ one trailing slash) pass; userinfo, path, query, fragment and any
# look-alike host do not.
host_re='^http://(localhost|127\.0\.0\.1|\[::1\])(:([0-9]{1,5}))?/?$'
if [[ $base =~ $host_re ]]; then
  port=${BASH_REMATCH[3]}
  { [ -z "${BASH_REMATCH[2]}" ] || { [ $((10#$port)) -ge 1 ] && [ $((10#$port)) -le 65535 ]; }; } || port=bad
else
  port=bad
fi
[ "$port" != bad ] || die "BASE_URL must be http://localhost, http://127.0.0.1 or http://[::1] with an optional port and nothing else; the admin key is sent there"
base=${base%/}
[ -n "${ADMIN_API_KEY:-}" ] || die "ADMIN_API_KEY is required (an admin API key: task bootstrap's, or a break-glass auth.api_keys entry)"
command -v jq >/dev/null || die "jq is required"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# printable <text> <secret>: the server's words are only echoed when they look like a code
# (lowercase, digits, _ . ? and at most 64 of them) and do not contain the credential just sent;
# a server that reflects the key or a token as its error code gets <redacted>.
printable() {
  if [[ $1 =~ ^[a-z0-9_.?]{1,64}$ ]] && [[ $1 != *"$2"* ]]; then printf '%s' "$1"; else printf '<redacted>'; fi
}

# rpc <Method> <json> <header-name> <header-value>: POSTs to the Connect endpoint and leaves the
# response in $resp. The credential header goes through curl's stdin config, so it never shows in a
# process list. On failure only the HTTP status, Connect code and x-error-code are reported.
resp=
rpc() {
  local method=$1 body=$2 name=$3 secret=$4 value status
  value=${secret//\\/\\\\}; value=${value//\"/\\\"}
  status=$(printf 'header = "%s: %s"\n' "$name" "$value" | curl -sS -K - -o "$work/body" -D "$work/headers" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data-binary "$body" "$base/$svc/$method") || die "$method: cannot reach $base"
  if [ "$status" != 200 ]; then
    local code xcode
    code=$(jq -r '.code // empty' "$work/body" 2>/dev/null || true)
    xcode=$(tr -d '\r' < "$work/headers" | sed -n 's/^[Xx]-[Ee]rror-[Cc]ode: *//p' | head -1)
    die "$method failed: http $(printable "$status" "$value"), code $(printable "${code:-?}" "$value"), x-error-code $(printable "${xcode:-?}" "$secret")"
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

# expect_account <id> <email> <roles-json>: fails closed unless the existing account is active and has
# exactly the expected roles. A same-named account made by hand (admin without its role, user holding
# admin) is never adopted, and its roles are never changed here.
expect_account() {
  admin GetAccount "$(jq -nc --arg i "$1" '{id: $i}')"
  jq -e --argjson want "$3" '.account | (.status // "ACCOUNT_STATUS_ACTIVE") == "ACCOUNT_STATUS_ACTIVE" and ((.roles // []) | sort) == ($want | sort)' <<<"$resp" >/dev/null \
    || die "the existing account $2 is disabled or its roles differ from $3; fix or rename it by hand (AssignRoles / EnableAccount) and rerun"
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
  else
    expect_account "$id" "$email" "$roles"
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

# Check every existing account before changing anything, so a bad one stops the run with nothing bound.
preflight() {
  local id
  id=$(find_account "$1")
  [ -z "$id" ] || expect_account "$id" "$1" "$(jq -nc '$ARGS.positional' --args "${@:2}")"
}
preflight admin@example.com "$role"
preflight user@example.com
prebind admin@example.com "$role"
prebind user@example.com
