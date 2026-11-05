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

[ "$fails" = 0 ] || { echo "FAIL ($fails)"; exit 1; }
echo "ok  parallel harnesses"
