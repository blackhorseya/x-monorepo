#!/usr/bin/env bash
# Which config file does each real `task` entry hand its binary? (#414, #358, #412)
#
#   scripts/task-config-test.sh     # needs go + task; CI's build job runs it after `task build`
#
# Every task that reads a config file chooses it from its own variable (CONFIG, CATALOG_CONFIG,
# PAYMENT_CONFIG). A parent APP_CONFIG already in the environment must not win over it: a Task `env:`
# entry does not beat the OS environment, so the choice has to be made in the command itself. The probe
# is the real entry: a config file that cannot be parsed, at a path with a space, chosen through the
# task's variable, while the parent environment names a different unparsable file. config.Load fails
# with `read APP_CONFIG=<the file it read>`, so the error says which file reached the binary. Nothing
# starts, nothing connects. Each entry is also run without a parent APP_CONFIG (positive control: the
# probe does see the chosen file when nothing competes).
set -uo pipefail
cd "$(dirname "$0")/.."

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/dir with space"
chosen="$tmp/dir with space/chosen.yaml"
parent="$tmp/parent.yaml"
printf 'http: [unterminated\n' >"$chosen"
printf 'http: [unterminated\n' >"$parent"

fail=0 pass=0
# entry: "<task> <VAR> [-- args...]"
entries=(
  "run CONFIG"
  "worker CONFIG"
  "poison CONFIG -- list"
  "expire CONFIG -- --older-than 1h --dry-run"
  "bootstrap CONFIG"
  "prune CONFIG -- --older-than 1h --dry-run"
  "migrate CONFIG -- version"
  "catalog-rpc CATALOG_CONFIG"
  "payment-rpc PAYMENT_CONFIG"
  "payment-worker PAYMENT_CONFIG"
)
for e in "${entries[@]}"; do
  # shellcheck disable=SC2206
  parts=($e)
  name=${parts[0]} var=${parts[1]} rest=("${parts[@]:2}")
  for mode in competing alone; do
    if [ "$mode" = competing ]; then
      out=$(APP_CONFIG="$parent" task "$name" "$var=$chosen" ${rest[@]+"${rest[@]}"} 2>&1)
    else
      out=$(env -u APP_CONFIG task "$name" "$var=$chosen" ${rest[@]+"${rest[@]}"} 2>&1)
    fi
    out=$(printf '%s' "$out" | sed 's/\x1b\[[0-9;]*m//g')
    if ! printf '%s' "$out" | grep -qF "read APP_CONFIG=$chosen"; then
      fail=$((fail + 1)); echo "FAIL: task $name ($mode): did not read $var; output:" >&2; printf '%s\n' "$out" | tail -4 >&2
    elif [ "$mode" = competing ] && printf '%s' "$out" | grep -qF "$parent"; then
      fail=$((fail + 1)); echo "FAIL: task $name ($mode): the parent APP_CONFIG reached the binary" >&2
    else
      pass=$((pass + 1))
    fi
  done
done
echo "scripts/task-config-test.sh: $pass passed, $fail failed"
[ "$fail" = 0 ]
