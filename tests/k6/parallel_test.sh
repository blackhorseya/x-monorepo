#!/usr/bin/env bash
# The parallel harnesses (#451, #463) must stay red when one member fails, and must never read a failing external command,
# a missing file or a stale file as success. Needs python3 and k6 (wire cases) only: smoke.sh runs against stub binaries
# (a python server that answers /api/readyz), `k6` is a shim there.
#   parallel_test.sh                 (from the repository root; `task k6-parallel-test`)
# Each case runs smoke.sh / wire.sh and states the exit it must have. The cases run PARALLEL_TEST_JOBS (default 8) at a
# time, each with a port range ($P … $P+39), a bin dir and a work dir of its own; a case without a recorded status is a FAIL.
#   smoke  every member alone failing, a failing middle member, partial lists
#   wire   k6 exits 0; a lane killed before it wrote a verdict; grep / cat / sleep / rm / mkdir / python3 / env exiting 42 after
#          partial or full output; verdict lines dropped; a second run whose k6 dies without writing its files while the
#          first run's files are still in the bin dir (the failure must name the missing files)
#   smoke  curl / sleep / tee / cat / env exiting 42 (seq is no longer called: catalog.sh counts in the shell, #462)
set -euo pipefail
abs_dir_of() { local p=${1%/*}; [ "$p" != "$1" ] || p=.; ( cd "$p${2:+/$2}" && pwd ); }   # the absolute directory of a path, without dirname inside $(…)
root=$(abs_dir_of "$0" ../..) || exit 1
. "$root/scripts/best-effort.sh"   # best_effort: diagnostics / cleanup / wait-gone only (#462)
cd "$root" || exit 1
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
jobs_max=${PARALLEL_TEST_JOBS:-8}
mkdir -p "$work/res"

# --- fixtures ---
cat > "$work/stub.py" <<'PY'
import sys, os, time
from http.server import BaseHTTPRequestHandler, HTTPServer
time.sleep(1.0)   # the first readiness poll must miss: a failing sleep/curl in the wait loop has to be able to matter
port = int(os.environ["APP_HTTP_ADDR"].lstrip(":"))
class H(BaseHTTPRequestHandler):
    def do_GET(self): self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", port), H).serve_forever()
PY
mkbin() { # <dir>: stub rpc / catalog-rpc / worker
  mkdir -p "$1"
  for b in rpc catalog-rpc; do printf '#!/bin/sh\nexec python3 %s/stub.py\n' "$work" > "$1/$b"; chmod +x "$1/$b"; done
  printf '#!/bin/sh\nsleep 300\n' > "$1/worker"; chmod +x "$1/worker"
}
shim() { # <name> <body…> → the dir of a shim called <name>
  local d="$work/shim-$1-$RANDOM"; mkdir -p "$d"; local n=$1; shift
  printf '#!/bin/sh\n%s\n' "$*" > "$d/$n"; chmod +x "$d/$n"; echo "$d"
}
# k6 shim for smoke: `k6 run --quiet tests/k6/<script>.js …` exits 99 when <script> is in $SHIM_FAIL, else 0
K6SMOKE=$(shim k6 'for a in "$@"; do case $a in tests/k6/*.js) s=${a#tests/k6/}; s=${s%.js};; esac; done
case " ${SHIM_FAIL:-} " in *" $s "*) echo "shim: $s fails"; exit 99;; esac; echo "shim: $s ok"')
# grep / cat shims for the wire cases (pass through unless GREP_SHIM / CAT_SHIM says otherwise)
GREPSHIM=$(shim grep 'real=/usr/bin/grep; [ -x "$real" ] || real=/bin/grep
case "$*" in
  *"transport\."*) case ${GREP_SHIM:-} in full) $real "$@"; exit 42;; partial) $real "$@" | head -n 1; exit 42;; esac;;
  *canary*) [ "${GREP_SHIM:-}" = leak ] && exit 42;;
esac
exec $real "$@"')
CATSHIM=$(shim cat 'case "$*" in *.out*) case ${CAT_SHIM:-} in unreadable) exit 42;; noverdict) /bin/cat "$@" | /usr/bin/grep -v -- "--- PASS"; exit 0;; partial) /bin/cat "$@" | head -n 1; exit 42;; esac;;
  *.rc*) [ "${CAT_SHIM:-}" = rc ] && { /bin/cat "$@"; exit 42; };; esac
exec /bin/cat "$@"')
KILLLANE=$(shim k6 'kill -9 "$(ps -o ppid= -p "$PPID" | tr -d " ")"')
EXIT0K6=$(shim k6 'exit 0')
DEADK6=$(shim k6 'for a in "$@"; do case $a in BASE_URL=*) u=${a#BASE_URL=};; esac; done; curl -s "$u/x" >/dev/null; echo "transport.malformed_response"; exit 42')
REAL_SLEEP=$(type -P sleep)

# --- case runners (run in a subshell of their own; $P, $D set by async) ---
wire() { # <env…>: wire.sh on orders (and ratelimit), 2 modes, no negatives, in $D/wbin
  env WIRE_NO_NEGATIVES=1 WIRE_MODES="status closed" PORT="$P" "$@" tests/k6/wire.sh "$D/wbin" orders ratelimit > "$D/wire.out" 2>&1
}
wire1() { env WIRE_NO_NEGATIVES=1 WIRE_MODES=status PORT="$P" "$@" tests/k6/wire.sh "$D/wbin" orders > "$D/wire.out" 2>&1; }
smoke() { # <SHIM_FAIL> <scripts…>
  local f=$1; shift; mkbin "$D/bin"
  PORT="$P" DATABASE_URL=x SHIM_FAIL=$f PATH="$K6SMOKE:$PATH" tests/k6/smoke.sh "$D/bin" "$@" > "$D/smoke.out" 2>&1
}
smoke1() { # <shim dir> <scripts…>: smoke.sh with one shimmed external command, k6 shim passing
  local sd=$1; shift; mkbin "$D/bin"
  PORT="$P" DATABASE_URL=x PATH="$sd:$K6SMOKE:$PATH" tests/k6/smoke.sh "$D/bin" "$@" > "$D/smoke.out" 2>&1
}
c_stale() { # real run, then a k6 that dies without writing its files, same bin dir
  wire1 || return 1
  PATH="$DEADK6:$PATH" wire1 && return 1
  grep -q "k6 did not write it in this call" "$D/wire.out"   # red for the target reason
}
c_negenv() { local sd; sd=$(shim env 'exit 42') || return 1; PATH="$sd:$PATH" env WIRE_NO_NEGATIVES= WIRE_MODES=status PORT="$P" tests/k6/wire.sh "$D/wbin" orders > "$D/wire.out" 2>&1; }
c_seqwire() { local sd; sd=$(shim "$1" 'echo 1; exit 42') || return 1; PATH="$sd:$PATH" wire1; }

# --- scheduling ---
names=() wants=() n=0
async() { # <name> <want zero|nonzero> <function> [args…]
  local name=$1 want=$2; shift 2
  local running
  until running=$(jobs -rp | wc -l | tr -d ' ') && [ "$running" -lt "$jobs_max" ]; do sleep 0.2; done
  local i=$n; n=$((n + 1)); names+=("$name"); wants+=("$want")
  (
    export P=$((21000 + 40 * i)) D="$work/c$i"; mkdir -p "$D"
    rc=0; "$@" || rc=$?
    echo "$rc" > "$work/res/$i"
  ) > "$work/res/$i.log" 2>&1 &
}

for s in "" reservation orders ratelimit timeout; do
  if [ -z "$s" ]; then async "smoke: all members pass" zero smoke ""; else async "smoke: $s alone fails" nonzero smoke "$s"; fi
done
async "smoke: orders fails, no reservation" nonzero smoke orders orders ratelimit timeout
async "smoke: ratelimit (middle) fails" nonzero smoke ratelimit orders ratelimit timeout
async "smoke: reservation only, passes" zero smoke "" reservation
async "smoke: three together, pass" zero smoke "" orders ratelimit timeout

withpath() { local d=$1; shift; PATH="$d:$PATH" "$@"; }   # <shim dir> <function…>: the shim first in PATH for that call

async "wire: real k6, 4 cells pass" zero wire
async "wire: k6 exits 0 for every cell" nonzero withpath "$EXIT0K6" wire
async "wire: a lane dies mid-cell" nonzero withpath "$KILLLANE" wire WIRE_JOBS=2
async "wire: grep exits 42 after full output" nonzero withpath "$GREPSHIM" wire1 GREP_SHIM=full
async "wire: grep exits 42 after partial output" nonzero withpath "$GREPSHIM" wire1 GREP_SHIM=partial
async "wire: grep exits 42 on the leak check" nonzero withpath "$GREPSHIM" wire1 GREP_SHIM=leak
async "wire: the grep shim in pass-through is still green" zero withpath "$GREPSHIM" wire1
async "wire: cat of every .out exits 42" nonzero withpath "$CATSHIM" wire1 CAT_SHIM=unreadable
async "wire: cat drops the verdict lines" nonzero withpath "$CATSHIM" wire1 CAT_SHIM=noverdict
async "wire: cat prints one line of a .out then exits 42" nonzero withpath "$CATSHIM" wire1 CAT_SHIM=partial
async "wire: cat prints a .rc then exits 42" nonzero withpath "$CATSHIM" wire1 CAT_SHIM=rc
async "wire: the cat shim in pass-through is still green" zero withpath "$CATSHIM" wire1
sd=$(shim sleep 'exit 42') || exit 1
async "wire: sleep exits 42" nonzero withpath "$sd" wire1
sd=$(shim sleep "$REAL_SLEEP 0.05; exit 42") || exit 1
async "wire: sleep exits 42 after waiting" nonzero withpath "$sd" wire1
sd=$(shim rm 'exit 42') || exit 1
async "wire: rm exits 42" nonzero withpath "$sd" wire1
sd=$(shim rm '/bin/rm "$@"; exit 42') || exit 1
async "wire: rm exits 42 after removing" nonzero withpath "$sd" wire1
sd=$(shim mkdir '/bin/mkdir "$@"; exit 42') || exit 1
async "wire: mkdir exits 42 after creating" nonzero withpath "$sd" wire1
sd=$(shim python3 'echo "served status" >&2; exit 42') || exit 1
async "wire: python3 (the server) prints its marker then exits 42" nonzero withpath "$sd" wire1
for c in seq sort tr basename; do   # no longer called by wire.sh: a failing one changes nothing
  sd=$(shim "$c" 'echo 1; exit 42') || exit 1
  async "wire: $c is not used (exit 42 shim, still green)" zero withpath "$sd" wire1
done
async "wire: env exits 42 (negative cells must not pass as 'red')" nonzero c_negenv
async "wire: second run, k6 dies without writing its files; the failure names the missing files" zero c_stale

sd=$(shim curl 'echo partial; exit 42') || exit 1
async "smoke: curl exits 42 (never ready)" nonzero smoke1 "$sd" orders
sd=$(shim sleep 'exit 42') || exit 1
async "smoke: sleep exits 42" nonzero smoke1 "$sd" orders
sd=$(shim tee '/bin/cat > /dev/null; exit 42') || exit 1
async "smoke: tee swallows its input and exits 42" nonzero smoke1 "$sd" reservation
sd=$(shim tee 'head -n 1 | /bin/cat > "$1"; exit 42') || exit 1
async "smoke: tee writes one line and exits 42" nonzero smoke1 "$sd" reservation
sd=$(shim cat 'case "$*" in *.out*) exit 42;; esac; exec /bin/cat "$@"') || exit 1
async "smoke: cat of a script's output exits 42" nonzero smoke1 "$sd" orders
sd=$(shim env 'exit 42') || exit 1
async "smoke: env (starts the rpc) exits 42" nonzero smoke1 "$sd" orders
# catalog.sh used to count its readiness loop with `for _ in $(seq 1 100)`: the status of seq was dropped, so a seq that printed
# 1 and died was a one-round wait (#462). It counts in the shell now, and a failing seq changes nothing.
sd=$(shim seq 'echo 1; exit 42') || exit 1
async "smoke: seq is not used by catalog.sh (exit 42 shim, still green)" zero smoke1 "$sd" orders

best_effort wait   # every case has its own status file below; a case that left none is a FAIL

fails=0
for i in "${!names[@]}"; do
  got=missing
  if [ -f "$work/res/$i" ]; then got=$(cat "$work/res/$i") || exit 1; fi
  want=${wants[$i]}
  if { [ "$want" = zero ] && [ "$got" = 0 ]; } || { [ "$want" = nonzero ] && [ "$got" != 0 ] && [ "$got" != missing ]; }; then
    echo "ok   ${names[$i]}"
  else
    echo "FAIL ${names[$i]}: exit $got, want $want"
    [ -f "$work/c$i/wire.out" ] && tail -n 5 "$work/c$i/wire.out" | sed 's/^/       | /'
    [ -f "$work/c$i/smoke.out" ] && tail -n 5 "$work/c$i/smoke.out" | sed 's/^/       | /'
    fails=$((fails + 1))
  fi
done
[ "$fails" = 0 ] || { echo "FAIL ($fails of ${#names[@]})"; exit 1; }
echo "ok  parallel harnesses (${#names[@]} cases)"
