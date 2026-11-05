#!/usr/bin/env bash
# The parallel harnesses (#451) must stay red when one member fails — a parallel runner that loses an exit
# status, or a lane that dies without a verdict, would turn the gate green. Needs python3 and k6 (wire cases)
# only: smoke.sh runs against stub binaries (a python server that answers /api/readyz), `k6` is a shim.
#   parallel_test.sh                 (from the repository root; `task k6-parallel-test`)
# Each case runs smoke.sh / wire.sh and states the exit it must have:
#   smoke  every member alone failing (shim k6 exits 99 for that script) → non-0; none failing → 0;
#          partial lists (reservation alone, orders+ratelimit+timeout without it) keep the same rule
#   wire   a k6 that exits 0 → non-0 (the cell's own check); a lane killed before it wrote a verdict → non-0
#          and the cell it never ran is printed as `--- FAIL:`
set -euo pipefail
cd "$(dirname "$0")/../.."
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
fails=0
check() { # <name> <want: zero|nonzero> <exit code>
  if { [ "$2" = zero ] && [ "$3" = 0 ]; } || { [ "$2" = nonzero ] && [ "$3" != 0 ]; }; then echo "ok   $1"
  else echo "FAIL $1: exit $3, want $2"; fails=$((fails + 1)); fi
}

# --- smoke.sh against stubs ---
bin=$work/bin; mkdir -p "$bin" "$work/shim"
cat > "$work/stub.py" <<'PY'
import sys, os
from http.server import BaseHTTPRequestHandler, HTTPServer
import time
time.sleep(0.4)   # the first readiness poll must miss: a failing sleep/curl in the wait loop has to be able to matter
port = int(os.environ["APP_HTTP_ADDR"].lstrip(":"))
class H(BaseHTTPRequestHandler):
    def do_GET(self): self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", port), H).serve_forever()
PY
for b in rpc catalog-rpc; do printf '#!/bin/sh\nexec python3 %s/stub.py\n' "$work" > "$bin/$b"; chmod +x "$bin/$b"; done
printf '#!/bin/sh\nsleep 300\n' > "$bin/worker"; chmod +x "$bin/worker"
# k6 shim: `k6 run --quiet tests/k6/<script>.js …` exits 99 when <script> is in $SHIM_FAIL, else 0
cat > "$work/shim/k6" <<'SH'
#!/bin/sh
for a in "$@"; do case $a in tests/k6/*.js) s=${a#tests/k6/}; s=${s%.js};; esac; done
case " ${SHIM_FAIL:-} " in *" $s "*) echo "shim: $s fails"; exit 99;; esac
echo "shim: $s ok"
SH
chmod +x "$work/shim/k6"
smoke() { # <SHIM_FAIL> <scripts…> → exit code of smoke.sh
  local f=$1; shift
  PORT=${PORT:-18400} DATABASE_URL=x SHIM_FAIL=$f PATH="$work/shim:$PATH" tests/k6/smoke.sh "$bin" "$@" > "$work/smoke.out" 2>&1 && return 0 || return $?
}
rc=0; smoke "" || rc=$?;                                      check "smoke: all members pass" zero "$rc"
for s in reservation orders ratelimit timeout; do
  rc=0; smoke "$s" || rc=$?;                                  check "smoke: $s alone fails" nonzero "$rc"
done
rc=0; smoke "orders" orders ratelimit timeout || rc=$?;       check "smoke: orders fails, no reservation" nonzero "$rc"
rc=0; smoke "ratelimit" orders ratelimit timeout || rc=$?;    check "smoke: ratelimit (middle) fails" nonzero "$rc"
rc=0; smoke "" reservation || rc=$?;                          check "smoke: reservation only, passes" zero "$rc"
rc=0; smoke "" orders ratelimit timeout || rc=$?;              check "smoke: three together, pass" zero "$rc"

# --- wire.sh ---
wire() { # <extra env…> → exit code
  env WIRE_NO_NEGATIVES=1 WIRE_MODES="status closed" PORT=18500 "$@" tests/k6/wire.sh "$work/wbin" orders ratelimit > "$work/wire.out" 2>&1 && return 0 || return $?
}
mkdir -p "$work/wbin"
rc=0; wire || rc=$?;                                          check "wire: real k6, 4 cells pass" zero "$rc"
printf '#!/bin/sh\nexit 0\n' > "$work/shim/k6"
rc=0; PATH="$work/shim:$PATH" wire || rc=$?;                  check "wire: k6 exits 0 for every cell" nonzero "$rc"
# a lane that dies after the k6 of its first cell started: the cell never gets a verdict
cat > "$work/shim/k6" <<'SH'
#!/bin/sh
kill -9 "$(ps -o ppid= -p "$PPID" | tr -d ' ')"
SH
rc=0; PATH="$work/shim:$PATH" wire WIRE_JOBS=2 || rc=$?;      check "wire: a lane dies mid-cell" nonzero "$rc"
grep -q '^--- FAIL: wire-' "$work/wire.out" && echo "ok   wire: the cell is printed as --- FAIL:" \
  || { echo "FAIL wire: no --- FAIL: line for the dead lane"; fails=$((fails + 1)); }

# grep failing AFTER writing output (all of it / part of it) or on the leak check must fail the cell, not read as
# "no match" / "no leak": the shim runs the real grep and then exits 42 (full), prints its first line only (partial),
# or exits 42 on the leak check (a pattern with `canary`)
cat > "$work/shim/grep" <<'SH'
#!/bin/sh
real=/usr/bin/grep; [ -x "$real" ] || real=/bin/grep
case "$*" in
  *'transport\.'*) case ${GREP_SHIM:-} in
    full) $real "$@"; $real "$@" >/dev/null; exit 42;;
    partial) $real "$@" | head -n 1; exit 42;;
  esac;;
  *canary*) [ "${GREP_SHIM:-}" = leak ] && exit 42;;
esac
exec $real "$@"
SH
chmod +x "$work/shim/grep"
rm -f "$work/shim/k6"   # the real k6 again for the cases below
rm -f "$work/shim/k6"
for m in full partial leak; do
  rc=0; PATH="$work/shim:$PATH" wire GREP_SHIM=$m WIRE_MODES=status || rc=$?
  check "wire: grep exits 42 ($m) is a FAIL" nonzero "$rc"
done
rc=0; PATH="$work/shim:$PATH" wire WIRE_MODES=status || rc=$?; check "wire: the grep shim in pass-through is still green" zero "$rc"

# the cell outputs wire.sh prints: unreadable (cat exits 42 for every .out), or readable but without any verdict line
# (cat drops the `--- PASS` lines, exit 0). Either way wire.sh must go red instead of printing nothing and `ok  wire gate`.
rm -f "$work/shim/grep"
cat > "$work/shim/cat" <<'SH'
#!/bin/sh
case "$*" in
  *.out*) case ${CAT_SHIM:-} in
    unreadable) exit 42;;
    noverdict) /bin/cat "$@" | /usr/bin/grep -v -- '--- PASS'; exit 0;;
  esac;;
esac
exec /bin/cat "$@"
SH
chmod +x "$work/shim/cat"
for m in unreadable noverdict; do
  rc=0; PATH="$work/shim:$PATH" wire CAT_SHIM=$m WIRE_MODES=status || rc=$?
  check "wire: cat shim ($m) is a FAIL" nonzero "$rc"
done
rc=0; PATH="$work/shim:$PATH" wire WIRE_MODES=status || rc=$?; check "wire: the cat shim in pass-through is still green" zero "$rc"

# --- every external command the two harnesses call: it fails (exit 42) after partial or full output → the run is red ---
# one shim per command, in a directory of its own (so the others stay real); `real` finds the genuine binary.
shim() { # <command> <body…> → prints the shim dir
  local d="$work/shim-$1"; mkdir -p "$d"; shift
  printf '#!/bin/sh\n%s\n' "$*" > "$d/$CMD"; chmod +x "$d/$CMD"; echo "$d"
}
withshim() { # <command> <body> <runner…>: runner with the shim first in PATH
  local c=$1 b=$2; shift 2; local d; CMD=$c d=$(shim "$c" "$b"); PATH="$d:$PATH" "$@"
}
REAL_SLEEP=$(command -v sleep)
rc=0; withshim sleep 'exit 42' wire WIRE_MODES=status || rc=$?;                   check "wire: sleep exits 42" nonzero "$rc"
rc=0; withshim sleep "$REAL_SLEEP 0.05; exit 42" wire WIRE_MODES=status || rc=$?; check "wire: sleep exits 42 after waiting" nonzero "$rc"
rc=0; withshim rm 'exit 42' wire WIRE_MODES=status || rc=$?;                      check "wire: rm exits 42" nonzero "$rc"
rc=0; withshim rm '/bin/rm "$@"; exit 42' wire WIRE_MODES=status || rc=$?;       check "wire: rm exits 42 after removing" nonzero "$rc"
rc=0; withshim mkdir '/bin/mkdir "$@"; exit 42' wire WIRE_MODES=status || rc=$?;  check "wire: mkdir exits 42 after creating" nonzero "$rc"
rc=0; withshim python3 'echo "served status" >&2; exit 42' wire WIRE_MODES=status || rc=$?; check "wire: python3 (the server) prints its marker then exits 42" nonzero "$rc"
rc=0; withshim cat 'case "$*" in *.out*) /bin/cat "$@" | head -n 1; exit 42;; esac; exec /bin/cat "$@"' wire WIRE_MODES=status || rc=$?; check "wire: cat prints one line of a .out then exits 42" nonzero "$rc"
rc=0; withshim cat 'case "$*" in *.rc*) /bin/cat "$@"; exit 42;; esac; exec /bin/cat "$@"' wire WIRE_MODES=status || rc=$?; check "wire: cat prints a .rc then exits 42" nonzero "$rc"
# seq, sort, tr, basename: no longer called; a failing one changes nothing (and wire still passes)
for c in seq sort tr basename; do
  rc=0; withshim "$c" 'echo 1; exit 42' wire WIRE_MODES=status || rc=$?;           check "wire: $c is not used (exit 42 shim, still green)" zero "$rc"
done
# env: the nested negative runs go through it; a failing env must not read as "red as it must"
rc=0; withshim env 'exit 42' env WIRE_NO_NEGATIVES= WIRE_MODES=status PORT=18500 tests/k6/wire.sh "$work/wbin2" orders > "$work/wire.out" 2>&1 || rc=$?
check "wire: env exits 42 (negative cells must not pass as 'red')" nonzero "$rc"

# a k6 that reaches the server, prints a line and dies without writing summary / metrics, in a bin that already holds a
# real run's files: judged by what the OLD call left, it would pass
mkdir -p "$work/wbin3"
rc=0; WIRE_NO_NEGATIVES=1 WIRE_MODES=status PORT=18600 tests/k6/wire.sh "$work/wbin3" orders > "$work/wire.out" 2>&1 || rc=$?; check "wire: first run (real k6) is green" zero "$rc"
rc=0; withshim k6 'for a in "$@"; do case $a in BASE_URL=*) u=${a#BASE_URL=};; esac; done; curl -s "$u/x" >/dev/null; echo "transport.malformed_response"; exit 42' \
  env WIRE_NO_NEGATIVES=1 WIRE_MODES=status PORT=18600 tests/k6/wire.sh "$work/wbin3" orders > "$work/wire.out" 2>&1 || rc=$?
check "wire: second run, k6 dies without writing its files (stale files from the first)" nonzero "$rc"
# red for the target reason: the cell says the files were not written by this call (not some other failure)
if grep -q "k6 did not write it in this call" "$work/wire.out"; then echo "ok   wire: the stale-file run fails on 'k6 did not write it in this call'"
else echo "FAIL wire: the stale-file run did not fail on the missing-artifact assertion"; fails=$((fails + 1)); fi

# smoke.sh: the same, per command
one() { SHIM_FAIL="" PORT=18700 DATABASE_URL=x tests/k6/smoke.sh "$bin" "$@" > "$work/smoke.out" 2>&1 && return 0 || return $?; }
rc=0; withshim curl 'echo partial; exit 42' one orders || rc=$?;                   check "smoke: curl exits 42 (never ready)" nonzero "$rc"
rc=0; withshim sleep 'exit 42' one orders || rc=$?;                                check "smoke: sleep exits 42" nonzero "$rc"
rc=0; withshim tee '/bin/cat > /dev/null; exit 42' one reservation || rc=$?;       check "smoke: tee swallows its input and exits 42" nonzero "$rc"
rc=0; withshim tee 'head -n 1 | /bin/cat > "$1"; exit 42' one reservation || rc=$?; check "smoke: tee writes one line and exits 42" nonzero "$rc"
rc=0; withshim cat 'case "$*" in *.out*) exit 42;; esac; exec /bin/cat "$@"' one orders || rc=$?; check "smoke: cat of a script's output exits 42" nonzero "$rc"
rc=0; withshim env 'exit 42' one orders || rc=$?;                                  check "smoke: env (starts the rpc) exits 42" nonzero "$rc"
# seq is still called by catalog.sh (sourced; not changed here): its readiness loop ends in a hard readiness check, so a short
# or failed seq ends red, never green
rc=0; withshim seq 'echo 1; exit 42' one orders || rc=$?;                          check "smoke: seq (catalog.sh) prints 1 and exits 42" nonzero "$rc"

[ "$fails" = 0 ] || { echo "FAIL ($fails)"; exit 1; }
echo "ok  parallel harnesses"
