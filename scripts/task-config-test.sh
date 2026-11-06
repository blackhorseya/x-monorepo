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
#
# Two behaviours, two kinds of cell, so that a failure names the one that broke:
#   priority  the chosen path has NO space, so the `config` seeding task passes and the binary reaches
#             config.Load: the only thing judged is which file it read.
#   quoting   `task config CONFIG=<path with a space>`, from the real entry: an existing file is left as it
#             is (exit 0, content unchanged), a missing one is seeded from configs/config.example.yaml.
#
# TASKFILE=<file in the repo root> runs the same cells against another Taskfile (default Taskfile.yml), to
# show them red on an older one: git show <old sha>:Taskfile.yml > Taskfile.old.yml (delete it afterwards).
set -uo pipefail
self_dir=${0%/*}; [ "$self_dir" != "$0" ] || self_dir=.   # dirname without a $(…)
cd "$self_dir/.." || exit 1

TASKFILE=${TASKFILE:-Taskfile.yml}
strip() { sed 's/\x1b\[[0-9;]*m//g'; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/dir with space"
chosen="$tmp/chosen.yaml"   # no space: priority cells
parent="$tmp/parent.yaml"
printf 'http: [unterminated\n' >"$chosen"
printf 'http: [unterminated\n' >"$parent"

fail=0 pass=0
# priority entries: "<task> <VAR> [-- args...]"
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
      out=$(APP_CONFIG="$parent" task -t "$TASKFILE" "$name" "$var=$chosen" ${rest[@]+"${rest[@]}"} 2>&1)
    else
      out=$(env -u APP_CONFIG task -t "$TASKFILE" "$name" "$var=$chosen" ${rest[@]+"${rest[@]}"} 2>&1)
    fi
    out=$(printf '%s' "$out" | strip)
    if ! printf '%s' "$out" | grep -qF "read APP_CONFIG=$chosen"; then
      fail=$((fail + 1)); echo "FAIL: priority: task $name ($mode): did not read $var=$chosen; output:" >&2; printf '%s\n' "$out" | tail -4 >&2
    elif [ "$mode" = competing ] && printf '%s' "$out" | grep -qF "$parent"; then
      fail=$((fail + 1)); echo "FAIL: priority: task $name ($mode): the parent APP_CONFIG reached the binary" >&2
    else
      pass=$((pass + 1))
    fi
  done
done

# quoting: the real `task config` entry with a path that has a space
spaced="$tmp/dir with space/local.yaml"
printf 'existing: content\n' >"$spaced"
out=$(task -t "$TASKFILE" config CONFIG="$spaced" 2>&1); rc=$?
spaced_content=$(cat "$spaced" 2>&1) || { echo "FAIL: quoting: cannot read $spaced after task config: $spaced_content" >&2; exit 1; }
if [ "$rc" = 0 ] && [ "$spaced_content" = 'existing: content' ]; then pass=$((pass + 1)); else
  fail=$((fail + 1)); echo "FAIL: quoting: task config on an existing file with a space: exit $rc, content [$spaced_content]" >&2; printf '%s\n' "$out" | strip | tail -3 >&2
fi
rm -f "$spaced"
out=$(task -t "$TASKFILE" config CONFIG="$spaced" 2>&1); rc=$?
if [ "$rc" = 0 ] && cmp -s "$spaced" configs/config.example.yaml; then pass=$((pass + 1)); else
  fail=$((fail + 1)); echo "FAIL: quoting: task config on a missing file with a space: exit $rc, seeded=$([ -f "$spaced" ] && echo yes || echo no)" >&2; printf '%s\n' "$out" | strip | tail -3 >&2
fi

echo "scripts/task-config-test.sh ($TASKFILE): $pass passed, $fail failed"
[ "$fail" = 0 ]
