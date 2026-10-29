#!/usr/bin/env bash
# bench.sh must only ever drop a database it created itself. A database that already has the name
# a run would pick (same second, same pid/suffix, or an old-format name) is somebody else's: the run
# must fail on CREATE DATABASE and leave it, with its data, untouched. `task k6-bench-test`; needs
# Postgres (DATABASE_URL) and the built binaries; not part of CI.
#   bench_db_test.sh <bin dir>
set -euo pipefail
bin=$1
base=${DATABASE_URL:-postgres://app:app@127.0.0.1:5432/app?sslmode=disable}
work=$(mktemp -d)
sentinels=()
trap 'for d in ${sentinels[@]+"${sentinels[@]}"}; do psql "$base" -qAt -c "drop database if exists $d with (force)" >/dev/null 2>&1 || true; done; rm -rf "$work"' EXIT

# a fixed clock, so the stamp bench.sh puts in the database name is known to the test
mkdir "$work/fakebin"
cat > "$work/fakebin/date" <<'EOF'
#!/bin/sh
echo 20000101T000000Z
EOF
chmod +x "$work/fakebin/date"

sentinel() { # <name>: a database with one row in it
  sentinels+=("$1")
  psql "$base" -qAt -c "drop database if exists $1 with (force)" -c "create database $1" >/dev/null
  psql "$(python3 -I -c 'import sys, urllib.parse as u
p = u.urlsplit(sys.argv[1]); print(u.urlunsplit(p._replace(path="/" + sys.argv[2])))' "$base" "$1")" \
    -qAt -c "create table keep(v int); insert into keep values (42)" >/dev/null
}
survives() { # <name>: still there, with its row
  local url
  url=$(python3 -I -c 'import sys, urllib.parse as u
p = u.urlsplit(sys.argv[1]); print(u.urlunsplit(p._replace(path="/" + sys.argv[2])))' "$base" "$1")
  [ "$(psql "$url" -qAt -c 'select v from keep' 2>/dev/null)" = 42 ]
}

fails=0
check() { if [ "$1" = ok ]; then echo "--- PASS: $2"; else echo "--- FAIL: $2"; fails=$((fails + 1)); fi; }

# 1. the exact name is taken (forced: fixed clock and suffix): the run fails and drops nothing
sentinel k6_bench_20000101t000000z_fixed_1
rc=0
PATH="$work/fakebin:$PATH" BENCH_DB_SUFFIX=fixed RUNS=3 CREATE_VUS=2 CREATE_DURATION=1s RESERVE_RATE=5 RESERVE_DURATION=2s \
  tests/k6/bench.sh "$bin" "$work/out1" > "$work/out1.log" 2>&1 || rc=$?
[ "$rc" != 0 ] && r=ok || r=bad; check $r "name taken: bench.sh exits non-zero (exit $rc)"
survives k6_bench_20000101t000000z_fixed_1 && r=ok || r=bad; check $r "name taken: the existing database and its row survive"

# 2. two runs in the same second (fixed clock, nothing else forced) do not pick the same name: the
# first-format name of the old scheme exists and is left alone, the bench completes
sentinel k6_bench_20000101t000000z_1
rc=0
PATH="$work/fakebin:$PATH" RUNS=3 CREATE_VUS=2 CREATE_DURATION=1s RESERVE_RATE=5 RESERVE_DURATION=2s \
  tests/k6/bench.sh "$bin" "$work/out2" > "$work/out2.log" 2>&1 || rc=$?
[ "$rc" = 0 ] && r=ok || r=bad; check $r "same second: bench.sh completes (exit $rc)"
survives k6_bench_20000101t000000z_1 && r=ok || r=bad; check $r "same second: the pre-existing old-format database survives"
left=$(psql "$base" -qAt -c "select count(*) from pg_database where datname like 'k6_bench_20000101t000000z_%' and datname <> 'k6_bench_20000101t000000z_1' and datname <> 'k6_bench_20000101t000000z_fixed_1'")
[ "$left" = 0 ] && r=ok || r=bad; check $r "same second: the run's own databases are dropped ($left left)"

[ "$fails" = 0 ] || { echo "FAIL: $fails check(s)"; tail -n 5 "$work/out1.log" "$work/out2.log"; exit 1; }
echo "ok  bench db safety"
