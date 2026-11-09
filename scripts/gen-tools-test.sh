#!/usr/bin/env bash
# A codegen task must not touch a file before it knows every tool it needs exists (#504).
#
#   scripts/gen-tools-test.sh      # needs task, go, jq and the five tools of `task tools`; CI's gen-freshness job runs it
#
# `task gen` used to run `buf format -w`, delete every generated *.pb.go / *.connect.go and only then call buf, so a
# worktree without ./bin/protoc-gen-connect-go ended up with 23 tracked files deleted. The tasks that use a ./bin tool now
# call the internal task `tools-check` first (NEED lists the tools; a missing or non-executable one exits 1 and names
# `task tools`). This script drives the REAL entries (`task -d <fixture> gen | gen-openapi | wire | check-gen-fresh |
# check-gen-fresh-all`) in a throwaway directory and judges three things per cell:
#   rc    the task exits non-zero (and not by the deadline)
#   hash  the SHA-256 of every file of the fixture (bin/ aside) is the same before and after
#   msg   the output names the missing tool and says `task tools`
# No git anywhere: the agend git shim redirects a fixture's git into the bound worktree, and a hash of the tree says
# "nothing was written" without it. The fixture carries sentinels: a generated Go file and two OpenAPI documents made
# stale on purpose, so any step that regenerates them changes the hash. Cells that never reach a write (the first command is
# the missing tool itself) can only go red on msg; the table below says which.
# A red on msg only shows that the failure is not diagnosable (no tool name, no `task tools`); it does not show that nothing is
# written. Only the hash cells (a plugin or wire missing behind gen, check-gen-fresh, check-gen-fresh-all) show that the old
# order wrote or deleted first.
#
# Controls (each one proves a verdict can change):
#   positive      every tool present: `gen`, `gen-openapi`, `check-gen-fresh`, `check-gen-fresh-all` DO change the hash
#                 (the harness sees writes) and never print the tools-check message; `wire` reaches `wire gen`.
#   mutants       for every cell, a copy of the Taskfile with that one tool removed from every NEED list (or, for the
#                 non-executable cells, the `-x` test turned into `-e`); the cell must go red on the assertion named in
#                 its row (hash where a write comes first, msg where none can).
#
# TASKFILE=<file>   judge another Taskfile (default Taskfile.yml), e.g. the one before the fix: git show <sha>:Taskfile.yml > /tmp/x
# MUTANTS=0         skip the mutants (the old Taskfile has no NEED lists to mutate)
# GEN_TOOLS_BIN=<dir>  where the five real tools are (default ./bin); they are symlinked into each fixture, never changed
set -uo pipefail
self_dir=${0%/*}; [ "$self_dir" != "$0" ] || self_dir=.   # dirname without a $(…)
cd "$self_dir/.." || exit 1
root=$PWD
. "$self_dir/best-effort.sh" || exit 1   # best_effort, capture

TASKFILE=${TASKFILE:-Taskfile.yml}
MUTANTS=${MUTANTS:-1}
TOOLS_SRC=${GEN_TOOLS_BIN:-$root/bin}
case $TASKFILE in /*) taskfile_abs=$TASKFILE ;; *) taskfile_abs=$root/$TASKFILE ;; esac

ALL_TOOLS="buf protoc-gen-go protoc-gen-connect-go protoc-gen-connect-openapi wire"
GEN_TOOLS="buf protoc-gen-go protoc-gen-connect-go protoc-gen-connect-openapi"
PLUGINS="protoc-gen-go protoc-gen-connect-go protoc-gen-connect-openapi"

set -m   # every child is its own process group, so a deadline can kill the whole tree, not only `task`
tmproot=${TMPDIR:-/tmp}; tmproot=${tmproot%/}
tmpl=gen-tools-test.XXXXXX
# the cleanup self-test below starts this script again with GEN_TOOLS_SELFTEST set; GEN_TOOLS_SELFTEST_TMPL is its mutant (a template the cleanup does not accept)
if [ -n "${GEN_TOOLS_SELFTEST:-}" ] && [ -n "${GEN_TOOLS_SELFTEST_TMPL:-}" ]; then tmpl=$GEN_TOOLS_SELFTEST_TMPL; fi
tmp=$(mktemp -d "$tmproot/$tmpl") || exit 1
CHILD=
cleanup() {
  if [ -n "$CHILD" ]; then best_effort kill -TERM -- "-$CHILD"; fi
  case $tmp in "$tmproot"/gen-tools-test.*) rm -rf "$tmp" ;; esac
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# self-test child: say where the temporary directory is, then end the way the parent asked (exit, or wait to be killed with TERM)
if [ -n "${GEN_TOOLS_SELFTEST:-}" ]; then
  printf '%s\n' "$tmp" >"$GEN_TOOLS_SELFTEST_OUT" || exit 1
  n=0
  if [ "$GEN_TOOLS_SELFTEST" = hold ]; then while [ "$n" -lt 300 ]; do sleep 0.1; n=$((n + 1)); done; fi
  exit 0
fi

for t in $ALL_TOOLS golangci-lint; do
  [ -x "$TOOLS_SRC/$t" ] || { echo "gen-tools-test: $TOOLS_SRC/$t is missing or not executable: run \`task tools\` (or set GEN_TOOLS_BIN)" >&2; exit 1; }
done
EXE=
find_exe() { # <name>: EXE = the first executable file of that name on PATH (parameter expansion and tests only, no $(…))
  local d IFS=:
  EXE=
  for d in $PATH; do
    if [ -f "$d/$1" ] && [ -x "$d/$1" ]; then EXE=$d/$1; return 0; fi
  done
  return 1
}
find_exe task || { echo "gen-tools-test: task is not on PATH" >&2; exit 1; }
task_bin=$EXE
find_exe go || { echo "gen-tools-test: go is not on PATH" >&2; exit 1; }
go_bin=$EXE
find_exe jq || { echo "gen-tools-test: jq is not on PATH" >&2; exit 1; }
jq_bin=$EXE
# The PATH of every run: task, go and jq (the Taskfile's MODULES variable needs the last two) and the system directories.
# Neither buf nor wire may be on it: the Taskfile falls back to a PATH copy of them, and a "missing" tool must really be missing.
mkdir -p "$tmp/path" || exit 1
ln -s "$task_bin" "$tmp/path/task" && ln -s "$go_bin" "$tmp/path/go" && ln -s "$jq_bin" "$tmp/path/jq" || exit 1
RUN_PATH="$tmp/path:/usr/bin:/bin:/usr/sbin:/sbin"
if PATH=$RUN_PATH command -v buf >/dev/null 2>&1 || PATH=$RUN_PATH command -v wire >/dev/null 2>&1; then
  echo "gen-tools-test: buf or wire is on the system PATH ($RUN_PATH): the missing-tool cells would not be missing" >&2
  exit 1
fi

# ── the minimal tree `task gen` / `gen-openapi` read ────────────────────────────────────────────────────────────────
base="$tmp/base"
mkdir -p "$base/scripts" || exit 1
for f in go.work buf.yaml buf.gen.yaml buf.lock scripts/modules.jq; do cp "$f" "$base/$f" || exit 1; done
cp -R protos "$base/protos" || exit 1
for d in pkgs/*/contract; do
  mkdir -p "$base/${d%/contract}" || exit 1
  cp -R "$d" "$base/$d" || exit 1
done
# sentinels: generated files made stale on purpose
SENT_GO=pkgs/identity/contract/v1/service.pb.go
SENT_OA1=pkgs/identity/contract/v1/openapi.yaml
SENT_OA2=pkgs/platform/contract/admin/v1/openapi.yaml
for s in $SENT_GO $SENT_OA1 $SENT_OA2; do
  [ -f "$base/$s" ] || { echo "gen-tools-test: sentinel $s is not in the repo" >&2; exit 1; }
  printf '\n# stale sentinel: only a regeneration removes this line\n' >>"$base/$s" || exit 1
done

snapshot() { # <dir> <outfile>: SHA-256 of every file except bin/, sorted
  ( cd "$1" && find . -path ./bin -prune -o -type f -print0 | sort -z | xargs -0 shasum -a 256 ) >"$2" </dev/null
}

RUN_RC=0
run_bounded() { # <seconds> <outfile> <command…>: RUN_RC is the exit status, 124 when the deadline killed it
  local secs=$1 out=$2 i=0 limit
  shift 2
  limit=$((secs * 10))
  RUN_RC=0
  "$@" >"$out" 2>&1 </dev/null &
  CHILD=$!
  while kill -0 "$CHILD" 2>/dev/null; do
    if [ "$i" -ge "$limit" ]; then
      best_effort kill -TERM -- "-$CHILD"
      sleep 2
      best_effort kill -KILL -- "-$CHILD"
      wait "$CHILD" 2>/dev/null
      CHILD=
      RUN_RC=124
      return 0
    fi
    sleep 0.1
    i=$((i + 1))
  done
  wait "$CHILD" || RUN_RC=$?
  CHILD=
  return 0
}

fx_n=0
FX=
FX_NAME="fx dir"   # a space in every fixture path: the Taskfile's BUF / WIRE / BIN_DIR must survive it (#504 r0)
PRESENT=
others() { # <tool to leave out>: PRESENT = the other tools of ALL_TOOLS
  local t
  PRESENT=
  for t in $ALL_TOOLS; do if [ "$t" != "$1" ]; then PRESENT="$PRESENT $t"; fi; done
}

# make_fixture <taskfile> <kind> [tool]: FX = a new fixture. kind none = every tool in bin/; missing = every tool but <tool>;
# nonexec = every tool, <tool> being a file the shell can see but not run (a stub, never a symlink to the real tool: chmod would reach it)
make_fixture() {
  local tf=$1 kind=$2 tool=${3:-} t
  fx_n=$((fx_n + 1))
  FX="$tmp/$FX_NAME.$fx_n"
  cp -R "$base" "$FX" || return 1
  cp "$tf" "$FX/Taskfile.yml" || return 1
  mkdir "$FX/bin" || return 1
  others "$tool"
  if [ "$kind" = none ]; then PRESENT=" $ALL_TOOLS"; fi
  for t in $PRESENT; do ln -s "$TOOLS_SRC/$t" "$FX/bin/$t" || return 1; done
  if [ "$kind" = nonexec ]; then
    printf '#!/bin/sh\nexit 0\n' >"$FX/bin/$tool" || return 1
    chmod 0644 "$FX/bin/$tool" || return 1
  fi
}

# make_mutant <kind> <tool> <outfile>: the Taskfile with the check for that cell taken away; refuses a mutant that changed nothing
make_mutant() {
  local kind=$1 tool=$2 out=$3
  if [ "$kind" = nonexec ]; then
    sed 's/\[ -x /[ -e /' "$taskfile_abs" >"$out" || return 1
  else
    sed "/NEED:/ s/ $tool / /g" "$taskfile_abs" >"$out" || return 1
  fi
  if cmp -s "$taskfile_abs" "$out"; then echo "gen-tools-test: the mutant ($kind $tool) changed nothing: no NEED list names it" >&2; return 1; fi
}

# run_cell <label> <entry> <tool> <kind> <taskfile>: CELL_FAIL = the assertions that failed (rc hash msg), LAST_RC
CELL_FAIL=
LAST_RC=0
run_cell() {
  local label=$1 entry=$2 tool=$3 kind=$4 tf=$5
  CELL_FAIL=setup
  make_fixture "$tf" "$kind" "$tool" || { echo "gen-tools-test: $label: cannot build the fixture" >&2; return 0; }
  snapshot "$FX" "$tmp/before" || { echo "gen-tools-test: $label: cannot hash the fixture" >&2; return 0; }
  # the hash is only evidence when it covers something, sentinels included
  if [ ! -s "$tmp/before" ] || ! grep -q "$SENT_GO" "$tmp/before" || ! grep -q "$SENT_OA1" "$tmp/before" || ! grep -q "$SENT_OA2" "$tmp/before"; then
    echo "gen-tools-test: $label: the before-snapshot is empty or lacks a sentinel" >&2
    return 0
  fi
  run_bounded 120 "$tmp/out" env PATH="$RUN_PATH" task -d "$FX" "$entry"
  LAST_RC=$RUN_RC
  snapshot "$FX" "$tmp/after" || { echo "gen-tools-test: $label: cannot hash the fixture after" >&2; return 0; }
  cp "$tmp/out" "$tmp/out.last" || return 1
  CELL_FAIL=
  if [ "$LAST_RC" = 0 ] || [ "$LAST_RC" = 124 ]; then CELL_FAIL="$CELL_FAIL rc"; fi
  if ! cmp -s "$tmp/before" "$tmp/after"; then CELL_FAIL="$CELL_FAIL hash"; fi
  if ! grep -q 'task tools' "$tmp/out" || ! grep -q -- "$tool" "$tmp/out"; then CELL_FAIL="$CELL_FAIL msg"; fi
  CELL_FAIL=${CELL_FAIL# }
}

fail=0 pass=0
ok()  { pass=$((pass + 1)); printf 'ok   %s\n' "$1"; }
bad() { # the tail of the task output that went with the failed cell, colours stripped, a newline of its own
  fail=$((fail + 1)); printf 'FAIL %s\n' "$1"
  if [ -s "$tmp/out.last" ]; then sed -e 's/\x1b\[[0-9;]*m//g' -e 's/^/     | /' "$tmp/out.last" | tail -8; echo; fi
}

# ── positive controls: every tool present ───────────────────────────────────────────────────────────────────────────
# gen / gen-openapi / check-gen-fresh(-all) must WRITE (the hash changes: the sentinels are regenerated, so the harness does see a
# write) and must not print the tools-check message. wire has nothing to regenerate in the fixture (no apps/): it must get as far
# as the wire binary, which fails on its own ("wire: …"). Run in a plain path and in one with a space.
POS_WHY=
positive() { # <entry> <taskfile>: returns 0 when the entry behaved, 1 (POS_WHY says why) when not
  local entry=$1 tf=$2 rc
  POS_WHY=
  make_fixture "$tf" none || { POS_WHY="cannot build the fixture"; return 1; }
  if [ "$entry" = lint ]; then
    add_mini_module "$FX" || { POS_WHY="cannot add the mini module"; return 1; }
    ln -s "$TOOLS_SRC/golangci-lint" "$FX/bin/golangci-lint" || { POS_WHY="cannot link golangci-lint"; return 1; }
  fi
  snapshot "$FX" "$tmp/before" || { POS_WHY="cannot hash"; return 1; }
  if [ ! -s "$tmp/before" ]; then POS_WHY="empty before-snapshot"; return 1; fi
  run_bounded 240 "$tmp/out" env PATH="$RUN_PATH" task -d "$FX" "$entry"
  rc=$RUN_RC
  snapshot "$FX" "$tmp/after" || { POS_WHY="cannot hash after"; return 1; }
  cp "$tmp/out" "$tmp/out.last" || return 1
  if [ "$rc" = 124 ]; then POS_WHY="deadline"; return 1; fi
  if grep -q 'task tools' "$tmp/out"; then POS_WHY="reported a tool missing although every tool is present"; return 1; fi
  if [ "$entry" = wire ]; then
    # wire's own error (the fixture has no apps/), not Task's echo of the command line, which names the path whether or not it runs
    if ! grep -q '^wire: ' "$tmp/out"; then POS_WHY="wire itself did not run"; return 1; fi
  elif [ "$entry" = breaking ]; then
    # buf itself ran and failed on its own (the fixture is no git repository, so its clone of .git fails): that message is buf's, not Task's
    if ! grep -q 'git repository' "$tmp/out"; then POS_WHY="buf itself did not run"; return 1; fi
  elif [ "$entry" = lint ]; then
    # buf lint, then go vet and golangci-lint on the one module of the mini go.work
    if [ "$rc" != 0 ] || ! grep -q '^0 issues\.' "$tmp/out"; then POS_WHY="lint did not finish with golangci-lint's 0 issues (exit $rc)"; return 1; fi
  elif cmp -s "$tmp/before" "$tmp/after"; then
    POS_WHY="the hash did not change although tools ran (the harness would not see a write)"; return 1
  fi
  return 0
}

# a go.work of one tiny module, so that `task lint` (MODULES comes from go.work) has something for go vet and golangci-lint to run on
add_mini_module() { # <fixture dir>
  mkdir "$1/m" || return 1
  printf 'go 1.21\n\nuse ./m\n' >"$1/go.work" || return 1
  printf 'module example.com/m\n\ngo 1.21\n' >"$1/m/go.mod" || return 1
  printf 'package main\n\nfunc main() {}\n' >"$1/m/main.go" || return 1
}

# the Taskfile as it was before the quoting fix, in the two places it matters: how BUF / WIRE are resolved, and how they are used
make_space_mutant() { # def|use <outfile>
  if [ "$1" = def ]; then
    # the definitions as they were before #505: the path spliced in bare
    sed -E 's/\{\{shellQuote \(printf "%s\/(buf|wire|golangci-lint)" \.BIN_DIR\)\}\}/{{.BIN_DIR}}\/\1/g' "$taskfile_abs" >"$2" || return 1
  else
    sed -e 's/{{shellQuote \.BUF}}/{{.BUF}}/g' -e 's/{{shellQuote \.WIRE}}/{{.WIRE}}/g' "$taskfile_abs" >"$2" || return 1
  fi
  if cmp -s "$taskfile_abs" "$2"; then echo "gen-tools-test: the space mutant ($1) changed nothing" >&2; return 1; fi
}

for entry in gen gen-openapi check-gen-fresh check-gen-fresh-all wire; do
  for FX_NAME in fx "fx dir"; do
    label="positive $entry (every tool present, path ${FX_NAME/ /<space>})"
    if positive "$entry" "$taskfile_abs"; then ok "$label"; else bad "$label: $POS_WHY"; fi
  done
  FX_NAME="fx dir"
  if [ "$MUTANTS" != 0 ]; then
    for m in def use; do
      make_space_mutant "$m" "$tmp/space-mutant.yml" || exit 1
      label="positive $entry (path with a space) — mutant: BUF/WIRE $m unquoted"
      if positive "$entry" "$tmp/space-mutant.yml"; then bad "$label: the mutant still passed"; else ok "$label — red: $POS_WHY"; fi
    done
  fi
done
FX_NAME="fx dir"

# ── shell metacharacters in the parent path (#507) ──────────────────────────────────────────────────────────────────
# The tool definitions (BUF / WIRE / GOLANGCI_LINT) put BIN_DIR into an sh: command. #505 wrapped it in double quotes, which a `"` in the
# path breaks (and `$x`, a backtick would be expanded there). Every cell: every tool present, the fixture under a parent whose name holds
# the character; the entry must find its tool and run it. The mutant of a cell puts the definitions back the way #505 wrote them
# ("<path>" in double quotes) or, for the one character double quotes survive, the naive fix ('<path>' in single quotes).
SPECIAL_NAMES=('d"q' "s'q" 'dol$lar' 'back`tick' "all\"'\$\`")
special_mutant() { # dq|sq <outfile>: the three definitions with the path in double / single quotes
  local q=\"
  if [ "$1" = sq ]; then q="'"; fi
  sed -E "s/\{\{shellQuote \(printf \"%s\/(buf|wire|golangci-lint)\" \.BIN_DIR\)\}\}/${q}{{.BIN_DIR}}\/\1${q}/g" "$taskfile_abs" >"$2" || return 1
  if cmp -s "$taskfile_abs" "$2"; then echo "gen-tools-test: the special-character mutant ($1) changed nothing" >&2; return 1; fi
}
special_entries() { # <name>: the entries judged under that name (every name: the five; the first and the last also breaking and lint)
  SPECIAL_E="gen gen-openapi check-gen-fresh check-gen-fresh-all wire"
  case $1 in 'd"q' | all*) SPECIAL_E="$SPECIAL_E breaking lint" ;; esac
}
SPECIAL_E=
for FX_NAME in "${SPECIAL_NAMES[@]}"; do
  special_entries "$FX_NAME"
  shape=dq
  if [ "$FX_NAME" = "s'q" ]; then shape=sq; fi
  for entry in $SPECIAL_E; do
    label="positive $entry (every tool present, path $FX_NAME)"
    if positive "$entry" "$taskfile_abs"; then ok "$label"; else bad "$label: $POS_WHY"; fi
    if [ "$MUTANTS" != 0 ]; then
      special_mutant "$shape" "$tmp/special-mutant.yml" || exit 1
      label="positive $entry (path $FX_NAME) — mutant: definitions with the path in $shape quotes"
      if positive "$entry" "$tmp/special-mutant.yml"; then bad "$label: the mutant still passed"; else ok "$label — red: $POS_WHY"; fi
    fi
  done
done
FX_NAME="fx dir"

# ── the temporary directory is gone afterwards (#504 r0) ────────────────────────────────────────────────────────────
# The script starts itself in self-test mode: it creates its temporary directory, says where it is, then exits (exit) or waits
# to be killed with TERM (hold); the directory must not exist afterwards. The mutant gives the child a template the cleanup does not accept.
SELF_PATH=
selftest_cleanup() { # <exit|hold> <template or empty>: SELF_PATH = the child's temporary directory
  local mode=$1 tmpl_env=$2 out="$tmp/selftest.out" i=0
  rm -f "$out"
  SELF_PATH=
  GEN_TOOLS_SELFTEST=$mode GEN_TOOLS_SELFTEST_OUT=$out GEN_TOOLS_SELFTEST_TMPL=$tmpl_env "$root/scripts/gen-tools-test.sh" >/dev/null 2>&1 </dev/null &
  CHILD=$!
  while [ ! -s "$out" ] && [ "$i" -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
  if [ ! -s "$out" ]; then
    best_effort kill -KILL -- "-$CHILD"
    wait "$CHILD" 2>/dev/null
    CHILD=
    return 1
  fi
  if [ "$mode" = hold ]; then best_effort kill -TERM -- "-$CHILD"; fi
  wait "$CHILD" 2>/dev/null
  CHILD=
  IFS= read -r SELF_PATH <"$out" || return 1
}
for mode in exit hold; do
  label="cleanup: the temporary directory is removed after $mode"
  if ! selftest_cleanup "$mode" ""; then bad "$label: the self-test child did not start"; continue; fi
  if [ -e "$SELF_PATH" ]; then
    bad "$label: $SELF_PATH is still there"
    case $SELF_PATH in "$tmproot"/tmp.* | "$tmproot"/gen-tools-test.*) rm -rf "$SELF_PATH" ;; esac
  else
    ok "$label"
  fi
  if [ "$MUTANTS" != 0 ]; then
    label="cleanup: the temporary directory is removed after $mode — mutant: a template the cleanup does not accept"
    if ! selftest_cleanup "$mode" "tmp.XXXXXX"; then bad "$label: the self-test child did not start"; continue; fi
    if [ -e "$SELF_PATH" ]; then
      ok "$label — red: $SELF_PATH was left"
      case $SELF_PATH in "$tmproot"/tmp.*) rm -rf "$SELF_PATH" ;; esac
    else
      bad "$label: the mutant left nothing behind, so the assertion cannot see a leak"
    fi
  fi
done

# ── the matrix: entry × tool removed (or made non-executable) ───────────────────────────────────────────────────────
# "red" is what the cell's mutant (the check for that tool taken away) must turn red: hash where a write comes before the failure,
# msg where none can (the missing tool is the first or the only command to run, so nothing is written before it fails).
cells=()
add_cells() { # <entry> <tools…>
  local entry=$1 t red
  shift
  for t in "$@"; do
    red="hash"
    case $entry in gen-openapi | wire) red=msg ;; esac
    if [ "$t" = buf ]; then red=msg; fi
    cells+=("$entry $t missing $red")
  done
}
add_cells gen $GEN_TOOLS
add_cells gen-openapi buf protoc-gen-connect-openapi
add_cells wire wire
add_cells check-gen-fresh $ALL_TOOLS
add_cells check-gen-fresh-all $ALL_TOOLS
for t in $PLUGINS; do cells+=("gen $t nonexec hash"); done

for c in "${cells[@]}"; do
  # shellcheck disable=SC2086
  set -- $c
  entry=$1 tool=$2 kind=$3 red=$4
  label="$entry × $kind $tool"
  run_cell "$label" "$entry" "$tool" "$kind" "$taskfile_abs" || exit 1
  if [ -z "$CELL_FAIL" ]; then ok "$label"; else bad "$label (failed: $CELL_FAIL; exit $LAST_RC)"; fi
  if [ "$MUTANTS" != 0 ]; then
    make_mutant "$kind" "$tool" "$tmp/mutant.yml" || exit 1
    run_cell "$label mutant" "$entry" "$tool" "$kind" "$tmp/mutant.yml" || exit 1
    case " $CELL_FAIL " in
      *" $red "*) ok "$label — mutant red on $red (failed: $CELL_FAIL)" ;;
      *) bad "$label — mutant must go red on $red, failed: [$CELL_FAIL]" ;;
    esac
  fi
done

echo "scripts/gen-tools-test.sh ($TASKFILE): $pass passed, $fail failed"
[ "$fail" = 0 ]
