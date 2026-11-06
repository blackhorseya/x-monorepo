#!/usr/bin/env bash
# The release's image for <app> starts: built from the binary the release dry run produced
# (bin/dist, `task release-snapshot`: the same goreleaser build the release ships) with the release's
# own deployments/docker/Dockerfile, run the way the chart runs it (non-root, read-only rootfs, every
# capability dropped), then asked over HTTP. CI's release-snapshot job runs it after the dry run.
#
#   task release-snapshot && scripts/image-smoke.sh payment-rpc     # or payment-worker
#
# What it proves for payment-rpc, and only that: the image holds a binary the dry run built, runs as a
# non-root user, `serve` comes up with a read-only rootfs, answers /api/readyz and /api/version (stamped,
# not "dev"), refuses a call without the service key and answers one with it from its own PaymentService
# (an unknown order is payment.not_found, not a 404 of a server that serves something else), and a run
# without a subcommand is refused. It says nothing about consume / expire (not shipped yet), Postgres or
# a provider: the container is in-memory with the mock provider.
#
# payment-worker (#410) has no listener, so its smoke is smaller and says less: the image holds a binary the dry run
# built, runs as a non-root user with a read-only rootfs and no capabilities, prints a stamped --version, refuses an
# unknown argument, and, started with no postgres.url, exits non-zero naming postgres.url (it got as far as its own
# start-up checks; it did not crash on the read-only rootfs). It does not prove it consumes: that needs Postgres.
set -euo pipefail
app=${1:?usage: scripts/image-smoke.sh <app>   (payment-rpc | payment-worker)}
self_dir=${0%/*}; [ "$self_dir" != "$0" ] || self_dir=.   # dirname without a $(…)
cd "$self_dir/.." || exit 1
. "$self_dir/best-effort.sh" || exit 1   # capture: the status of a container-engine call next to what it printed

case $app in
  payment-rpc) port=8082 args=(serve) ;;
  payment-worker) port="" args=() ;;
  *) echo "image-smoke: no smoke for $app" >&2; exit 2 ;;
esac

engine=docker
command -v docker >/dev/null 2>&1 || engine=podman
arch=$(go env GOARCH) || exit $?
bin=$(find bin/dist -type f -path "*/${app}_linux_${arch}*/${app}" 2>/dev/null | head -n1) || exit $?
if [ -z "$bin" ]; then
  echo "image-smoke: FAIL no linux/$arch $app binary under bin/dist: the release dry run (task release-snapshot) does not build it" >&2
  exit 1
fi

ctx=$(mktemp -d) || exit $?
cid=""
cleanup() {
  [ -z "$cid" ] || "$engine" rm -f "$cid" >/dev/null 2>&1 || true
  rm -rf "$ctx"
}
trap cleanup EXIT
mkdir -p "$ctx/linux/$arch"
cp "$bin" "$ctx/linux/$arch/$app"

# The release's Dockerfile and context layout (<os>/<arch>/<app>); TARGETPLATFORM is set by hand because
# a plain `docker build` / podman does not inject it the way buildx does (the same as `task helm-kind-up`).
image="localhost/x-monorepo/$app:smoke"
"$engine" build -q -f deployments/docker/Dockerfile --build-arg "APP=$app" --build-arg "TARGETPLATFORM=linux/$arch" -t "$image" "$ctx" >/dev/null

capture user user_rc "$engine" image inspect --format '{{.Config.User}}' "$image" || exit 1
[ "$user_rc" = 0 ] || { echo "image-smoke: FAIL cannot inspect the image" >&2; exit 1; }
case $user in
  ''|0|root|0:*|root:*) echo "image-smoke: FAIL the image runs as '$user', want a non-root user" >&2; exit 1 ;;
esac
echo "ok: runs as $user"

if [ "$app" = payment-worker ]; then
  run=("$engine" run --rm --read-only --cap-drop ALL --security-opt no-new-privileges -e APP_LOG_LEVEL=warn)
  capture version_raw version_rc "${run[@]}" "$image" --version || exit 1
  [ "$version_rc" = 0 ] || { echo "image-smoke: FAIL --version exited $version_rc" >&2; exit 1; }
  version=$(printf '%s\n' "$version_raw" | sed -n 's/^payment-worker version \([^ ]*\) .*/\1/p') || exit 1
  case $version in
    ''|dev) echo "image-smoke: FAIL --version says '$version': the binary is not stamped by the release build" >&2; exit 1 ;;
  esac
  echo "ok: stamped version $version"
  out=$(mktemp) || exit $?
  if "${run[@]}" "$image" --nope >"$out" 2>&1; then
    echo "image-smoke: FAIL the image started with an unknown argument" >&2; exit 1
  fi
  grep -q 'unknown argument' "$out" || { cat "$out" >&2; echo "image-smoke: FAIL an unknown argument was not refused by name" >&2; exit 1; }
  echo "ok: an unknown argument is refused"
  if "${run[@]}" "$image" >"$out" 2>&1; then
    echo "image-smoke: FAIL the image started with no postgres.url" >&2; exit 1
  fi
  grep -q 'postgres.url' "$out" || { cat "$out" >&2; echo "image-smoke: FAIL no postgres.url was not refused by name" >&2; exit 1; }
  rm -f "$out"
  echo "ok: no postgres.url is refused by name (read-only rootfs, no capabilities)"
  echo "image-smoke: $app OK"
  exit 0
fi

# No subcommand: refused, non-zero (a typo must not start the service).
if "$engine" run --rm --read-only --cap-drop ALL "$image" >/tmp/image-smoke-nosub.$$ 2>&1; then
  echo "image-smoke: FAIL the image started without a subcommand" >&2; exit 1
fi
rm -f /tmp/image-smoke-nosub.$$
echo "ok: no subcommand is refused"

key=image-smoke-service-key-0123456789
capture cid cid_rc "$engine" run -d --read-only --cap-drop ALL --security-opt no-new-privileges \
  -p "127.0.0.1::$port" \
  -e "APP_HTTP_ADDR=:$port" -e APP_LOG_LEVEL=warn \
  -e APP_AUTH_MODE=required -e "APP_AUTH_API_KEYS=rpc=$key:admin" \
  "$image" "${args[@]}" || exit 1
[ "$cid_rc" = 0 ] || { echo "image-smoke: FAIL cannot start the container" >&2; exit 1; }
capture port_out port_rc "$engine" port "$cid" "$port/tcp" || exit 1
[ "$port_rc" = 0 ] || { echo "image-smoke: FAIL no published port" >&2; exit 1; }
hostport=$(printf '%s\n' "$port_out" | head -n1 | sed 's/.*://') || exit 1
base="http://127.0.0.1:$hostport"

ready=""
for ((_try100 = 1; _try100 <= 60; _try100++)); do
  capture code poll_rc curl -s -o /dev/null -w '%{http_code}' "$base/api/readyz" || exit 1
  [ "$poll_rc" = 0 ] || echo "image-smoke: readyz poll $_try100: curl exited $poll_rc, retrying" >&2
  if [ "$code" = 200 ]; then ready=1; break; fi
  capture running running_rc "$engine" inspect --format '{{.State.Running}}' "$cid" || exit 1
  [ "$running_rc" = 0 ] || { echo "image-smoke: FAIL cannot inspect the container" >&2; exit 1; }
  [ "$running" = true ] || { "$engine" logs "$cid" >&2; echo "image-smoke: FAIL the container exited" >&2; exit 1; }
  sleep 0.5
done
[ -n "$ready" ] || { "$engine" logs "$cid" >&2; echo "image-smoke: FAIL /api/readyz never answered 200" >&2; exit 1; }
echo "ok: $app ${args[*]} is ready (read-only rootfs, no capabilities)"

version=$(curl -s "$base/api/version" | sed -n 's/.*"version" *: *"\([^"]*\)".*/\1/p') || exit $?
case $version in
  ''|dev) echo "image-smoke: FAIL /api/version says '$version': the binary is not stamped by the release build" >&2; exit 1 ;;
esac
echo "ok: stamped version $version"

body='{"orderId":"image-smoke-order","customerId":"image-smoke-customer"}'
noauth=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$body" "$base/payment.v1.PaymentService/GetPayment") || exit $?
[ "$noauth" = 401 ] || { echo "image-smoke: FAIL a call without the service key answered $noauth, want 401" >&2; exit 1; }
hdrs=$(mktemp) || exit $?
code=$(curl -s -D "$hdrs" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -H "X-API-Key: $key" -d "$body" "$base/payment.v1.PaymentService/GetPayment") || exit $?
xcode=$(tr -d '\r' <"$hdrs" | sed -n 's/^[Xx]-[Ee]rror-[Cc]ode: *//p') || exit $?
rm -f "$hdrs"
[ "$code" = 404 ] && [ "$xcode" = payment.not_found ] || { echo "image-smoke: FAIL GetPayment with the service key answered $code / '$xcode', want 404 / payment.not_found" >&2; exit 1; }
echo "ok: PaymentService answers (401 without the service key, payment.not_found for an unknown order with it)"
echo "image-smoke: $app OK"
