#!/usr/bin/env bash
# Static gate for #462: no shell script in the repo may throw away an external command's exit status without saying why.
# A gate that swallows a status is green when the command it asked about failed (#453/#455, #457 r0, #453 r1, #458).
#
#   scripts/check-exit-swallow.sh              # every tracked *.sh except scripts/hooks/ (task check-exit-swallow)
#   scripts/check-exit-swallow.sh <file>...    # only these files (scripts/test.sh's fixtures); no scripts/hooks/ skip here, and a file inside the repo is
#                                              # named by its repo-relative path however it was spelled (./x, ../x, absolute)
#
# Two layers. The `default` and `fnctx` rules (below) are judged on a SYNTAX TREE by tools/swallowast (Go, mvdan.cc/sh/v3/syntax; built once into ./bin/swallowast),
# the rest per line (comment lines and heredoc bodies are skipped):
#   swallow   `cmd || true`, `cmd || :`, `cmd || exit 0`, `cmd || return 0` — also inside $(...), a trap string or sh -c '...'
#   besteffort best_effort inside $(…) or on the right of an assignment: it hides the status of a command whose output is used
#   pipeline  `a | b` in a file that never says `set -o pipefail` (or set -euo pipefail ...): only b's status counts
#   procsub   `done < <(cmd)`, `diff <(a) <(b)`: the status of a process substitution is never seen, even under set -e / pipefail
#   rcbranch  `v=$(cmd) || rc=$?` whose output $v is used: $rc must be read (before $v is) by lines that can refuse it (return / exit / continue /
#             break; no function names, and a new value for $v is the failure swallowed; top-level code only: inside a function the fnctx shapes apply). Heuristic: it checks that a refusing branch EXISTS, not that it is right. Only scripts/test.sh itself is exempt (a test.sh elsewhere is scanned).
#   setargs   `set -- $(cmd)`, forlist `for x in $(cmd)`, argsub `expect_eq x "$(… | grep -c y)"`, declsub `local n="$(cmd)"` / `export X=$(cmd)` (also shellcheck SC2155):
#             the substitution stands where its status is lost. These positions are outside the `default` whitelist as well, so they are red there too and
#             `# exit-ok:` cannot make them green any more (#472).
#   default   (#470, #471, #472) a command substitution ($(…) or a backquote) is accepted ONLY as (1) the complete right-hand side of a plain assignment, `x=$(…)`
#             or `x="$(…)"`, or (2) the label idiom `$([ test ] && echo a || echo b)` (no substitution inside the test; anywhere). Every other position is red: an
#             argument, command name, redirection target, heredoc / here-string, for list, case word / pattern, [[ ]] / [ ] operand, ${x:-$(…)}, array element,
#             declaration, arithmetic, let, prefix assignment, process substitution, and a substitution inside a substitution. What stands inside one is a closed
#             whitelist too: ONE `&&` / `|` chain of simple commands whose first word is a LITERAL and not command / builtin / exec / eval / . / source (their options are not
#             parsed), optionally ending in `|| exit [N]` (no `;`, second statement, `!`, `&`, subshell, group, if / case / loop / function / [[ ]] / time / coproc). The
#             handler after an assignment, `x=$(…) || H`: H is `exit` / `return` (bare, or `$?` straight after the `||`), `exit N` / `return N`, or `{ …; exit N; }` /
#             `{ …; return N; }` (N a literal 1..255; the group's last statement; every earlier one is a literal echo / printf with no prefix assignment and no substitution in
#             an argument or redirection: a function call is not), or at top level `NAME=$?` (rcbranch judges it); `return` never inside a substitution. The handler must END its AND-OR
#             list (only `;` or a comment may follow; `&`, `|`, `&&`, `||` after it are red) and the list must not run in the background, as a pipeline operand or in a
#             coprocess. An assignment that is the condition of `if` / `elif` is accepted when what runs after it FAILED is the final `else` ending in `exit N` / `return N`
#             (bare / `$?` only when it is the else block's only statement), or an `elif` whose whole condition is an assignment `name=$(…)` to the same variable;
#             any other `elif` is red; `until`, always; `while`, and `! x=$(…)`, never.
#             A plain top-level `x=$(…)` (no handler, not a condition) relies on `set -e`: it is accepted only when the file turns errexit on at top level BEFORE it and
#             nothing in the file may turn it off (`set +e`, `set +o errexit`, `shopt -u`, a computed `set`), and every ancestor is a place where errexit is in force
#             (file / `{ }` / `( )` / then, else, do, case bodies; never a condition, the left of && ||, `!`, `&`, a pipeline operand, `time`, `coproc`, `$( )`). The one exempt node is the kind scripts'
#             `. "$(dirname "$0")/../../../scripts/best-effort.sh"`, matched field by field. Not exemptible.
#   fnctx     inside ANY function body, `x=$(…)` must carry that handler (`|| return` / `|| exit` with `$?` or a literal, or `|| { …; return N; }`; no `|| NAME=$?`),
#             or be the condition form above; `x=$(cmd) && use` is red there (at top level it is the documented way to use a value only when it succeeded).
#             A shape rule: it does not look at how or from where the function is called (`f || x`, `if f`, `$(f)`, a wrapper, eval, command f, another file, never) -
#             errexit may be off in any of those and the checker is never one spelling behind that way. Nothing is exemptible.
#   Workflow / composite-action files: the `run:` scripts are cut out (scripts/extract-run-blocks.py) and scanned with these same rules;
#             a step with no `shell:` runs `bash -e {0}` (no pipefail), `shell: bash` runs with pipefail, so the first is held to `pipeline`.
# Not flagged without a comment: `kill ... || true`, `rm ... || true` and `... kill ... && wait ... || true` (cleanup:
# an already-gone process or file is not a verdict). For the line rules swallow / besteffort / pipeline / procsub / rcbranch anything else needs either a fix or, on the SAME line, a comment
#   # exit-ok: <why this command's status cannot change the verdict>
# The fixes: compute the value first, `n=$(wc -l < f | tr -d ' ') || exit 1`, then use "$n"; `capture out rc cmd …` / `value_of -ok N cmd …` (scripts/best-effort.sh) where
# the status matters or a non-zero one is expected; inside a function only the handler shapes above. docs/ci.md has the rules and the reasons.
#
# `pipeline`, `procsub`, `swallow` and `rcbranch` are line scanners, not a shell parser: `pipeline` only looks at files without pipefail, and a `|` inside a quoted
# string is ignored. Exit 0 = clean, 1 = findings, 2 = the checker itself failed (never reported as clean).
set -euo pipefail
die() { echo "check-exit-swallow: $*" >&2; exit 2; }

here=$(dirname "$0") || { echo "check-exit-swallow: dirname failed" >&2; exit 2; }
cd "$here/.." || { echo "check-exit-swallow: cannot cd to the repo root from [$here]" >&2; exit 2; }
tmp=$(mktemp -d) || { echo "check-exit-swallow: mktemp failed" >&2; exit 2; }
trap 'rm -rf "$tmp"' EXIT

# an explicit argument that names a file inside this repo is turned into its repo-relative path (./scripts/test.sh, scripts/../scripts/test.sh and
# the absolute path all become scripts/test.sh), so the one path-based exemption (rcbranch / scripts/test.sh) does not depend on how it was
# spelled; a file outside the repo keeps the string it was given. Relative arguments are read from the repo root (the cd above).
root=$(pwd -P) || { echo "check-exit-swallow: pwd failed" >&2; exit 2; }
# no dirname / basename / realpath here: a command whose output renames the file to scan and which then dies would be a status swallowed (#466 r3);
# parameter expansion and the cd / pwd builtins cannot fail silently, and a cd that fails on a file that exists stops the run
pwd_of() { ( cd "$1" && pwd -P ); }   # the physical path of a directory, without `cd … && pwd` inside $(…)
repo_rel() {
  local d b
  [ -f "$1" ] || { printf '%s' "$1"; return; }
  case $1 in */*) d=${1%/*} b=${1##*/} ;; *) d=. b=$1 ;; esac
  d=$(pwd_of "${d:-/}") || { echo "check-exit-swallow: cannot cd to the directory of [$1]" >&2; exit 2; }
  case $d in
    "$root") printf '%s' "$b" ;;
    "$root"/*) printf '%s' "${d#"$root"/}/$b" ;;
    *) printf '%s' "$1" ;;
  esac
}

files=() yml=()
if [ "$#" -gt 0 ]; then
  for f in "$@"; do
    f=$(repo_rel "$f")
    case $f in *.yml | *.yaml) yml+=("$f") ;; *) files+=("$f") ;; esac
  done
else
  rc=0; git ls-files -- '*.sh' > "$tmp/ls" || rc=$?
  [ "$rc" = 0 ] || { echo "check-exit-swallow: git ls-files failed (exit $rc)" >&2; exit 2; }
  while IFS= read -r f; do
    case $f in scripts/hooks/*) continue ;; esac
    files+=("$f")
  done < "$tmp/ls"
  [ "${#files[@]}" -ge 20 ] || { echo "check-exit-swallow: only ${#files[@]} scripts to check — git ls-files '*.sh' no longer returns what it used to" >&2; exit 2; }
  rc=0; git ls-files -- '.github/workflows/*.yml' '.github/actions/*/action.yml' > "$tmp/ls" || rc=$?
  [ "$rc" = 0 ] || { echo "check-exit-swallow: git ls-files (workflows) failed (exit $rc)" >&2; exit 2; }
  while IFS= read -r f; do yml+=("$f"); done < "$tmp/ls"
  [ "${#yml[@]}" -ge 5 ] || { echo "check-exit-swallow: only ${#yml[@]} workflow / action files to check — the .github layout is not what it used to be" >&2; exit 2; }
fi
nscripts=${#files[@]} nyml=${#yml[@]}

# `run:` scripts of workflows and composite actions are cut out and scanned like any other script; map.tsv turns a finding in the
# extracted file back into a line of the YAML (extracted line k is YAML line first+k-2: line 1 is the `set` line the runner implies).
: > "$tmp/map.tsv"
if [ "${#yml[@]}" -gt 0 ]; then
  mkdir "$tmp/runs" || { echo "check-exit-swallow: mkdir failed" >&2; exit 2; }
  rc=0; python3 -I "$here/extract-run-blocks.py" "$tmp/runs" "${yml[@]}" > "$tmp/map.tsv" 2> "$tmp/extract.err" || rc=$?
  if [ "$rc" != 0 ]; then em=$(head -2 "$tmp/extract.err") || exit 2; die "extract-run-blocks.py failed (exit $rc): $em"; fi
  while IFS=$'\t' read -r sh _ _; do files+=("$sh"); done < "$tmp/map.tsv"
fi

# nothing to scan (a workflow whose only run: steps are not shell, e.g. `shell: python`): clean, and awk / shellcheck must not be started with no file
if [ "${#files[@]}" -eq 0 ]; then echo "check-exit-swallow: no shell to check in the ${nyml} workflow / action file(s)"; exit 0; fi

cat > "$tmp/scan.awk" <<'AWK'
# Reads the files named on the command line. Prints "file:line: rule: code" per finding; exit 1 if there was one.
function reset_file() { term = ""; dash = 0; hasflag = 0 }
# split the line into CODE (no trailing comment) and CMT (the comment, if any), tracking quotes
function split_comment(s,   i, c, q, n) {
  CODE = s; CMT = ""; q = ""; n = length(s)
  for (i = 1; i <= n; i++) {
    c = substr(s, i, 1)
    if (q == "") {
      if (c == "\\") { i++; continue }
      if (c == "\047" || c == "\"") { q = c; continue }
      if (c == "#" && (i == 1 || substr(s, i - 1, 1) ~ /[ \t]/)) { CODE = substr(s, 1, i - 1); CMT = substr(s, i); return }
    } else if (c == q) { q = "" }
    else if (q == "\"" && c == "\\") { i++ }
  }
}
# the first word of the command that ends right before position p of CODE ("" when there is none)
function verb_before(p,   s, w) {
  s = substr(CODE, 1, p - 1)
  while (match(s, /\$\([^()]*\)/) || match(s, /\$\{[^{}]*\}/)) s = substr(s, 1, RSTART - 1) "X" substr(s, RSTART + RLENGTH)   # arguments like "$(cat f)" and "${P##* }"
  while (match(s, /(;|&&|\|\||\$\(|\{|\(|[ \t]do[ \t]|[ \t]then[ \t]|\047)/)) s = substr(s, RSTART + RLENGTH)
  sub(/^[ \t]+/, "", s)
  if (match(s, /^"\$[A-Za-z_{}0-9]+"[ \t]+/)) s = substr(s, RLENGTH + 1)   # "$tool" rm -f ...
  w = s; sub(/[ \t].*$/, "", w)
  return w
}
function report(rule) { printf "%s:%d: %s: %s\n", FILENAME, FNR, rule, line; n++ }
# 1 when best_effort sits where its output would be USED: inside a $(…) / backticks, or on the right of an assignment. It is for
# diagnostics, cleanup and "already gone" waits only (scripts/best-effort.sh); a value or a status that matters takes an explicit branch.
function besteffort_misuse(c) {
  return (c ~ /(\$\(|`)[^)`]*best_effort/ || c ~ /[A-Za-z_][A-Za-z_0-9]*=("|\047)?best_effort/)
}
# 1 when a declaration builtin (local / declare / typeset / export / readonly) is followed, before any `;` `&&` `||` on the line, by a
# command substitution $(…) ($((…)) is arithmetic and does not count): `local n="$(cmd)"`, `declare -r n=$(cmd)`, `export X=$(cmd)`.
# The declaration is the command, and its own success hides the status of cmd (reviewer of #464: grep prints 1, exits 2, the
# function carries on with n=1). Deliberately NOT a parser of the assignments before it — regexes over shell quoting were
# beaten twice — it blocks anything that puts a $( after the builtin; `local n; n=$(cmd)` is fine because the `;` comes first.
# The cases a line scanner cannot see (a continuation line, a `;` inside a quoted value) are caught by the shellcheck SC2155 pass.
function declsub(c,   m, rest, p, q, t) {
  if (!match(c, /(^|[;&|{(]|[ \t])(local|declare|typeset|export|readonly)([ \t]|$)/)) return 0
  rest = substr(c, RSTART + RLENGTH)
  t = rest; gsub(/\$\(\(/, "XX", t)
  p = index(t, "$(")
  if (p == 0) return 0
  q = 0
  if (match(t, /(;|&&|\|\|)/)) q = RSTART
  return (q == 0 || p < q)
}
# 1 when CODE has a $(…) that is an ARGUMENT (not the right-hand side of an assignment) and counts matches inside it:
# `expect_eq x "$(… | grep -c y)"`, `[ "$(grep -c y f)" -ge 2 ]`. The status of a substitution used as an argument never reaches
# set -e, so a grep that dies is just a number (#456 r0).
function argsub(c,   i, j, d, ch, pre, inner, n) {
  n = length(c)
  for (i = 1; i < n; i++) {
    if (substr(c, i, 2) != "$(" || substr(c, i, 3) == "$((") continue
    pre = substr(c, 1, i - 1)
    if (pre ~ /=("|\047)?$/) continue                      # name=$(…) / name="$(…)": an assignment, set -e sees it
    d = 1
    for (j = i + 2; j <= n; j++) { ch = substr(c, j, 1); if (ch == "(") d++; else if (ch == ")") { d--; if (d == 0) break } }
    inner = substr(c, i + 2, j - i - 2)
    if (inner ~ /(grep[ \t]+-[A-Za-z]*c|count_lines)[ \t]/) return 1
  }
  return 0
}
FNR == 1 { reset_file() }
{
  line = $0
  if (term != "") {
    t = line; if (dash) sub(/^\t+/, "", t)
    if (t == term) term = ""
    next
  }
  if (line ~ /^[ \t]*#/) next
  tmp = line; gsub(/<<</, "", tmp)
  if (match(tmp, /<<-?[ \t]*\\?["\047]?[A-Za-z_][A-Za-z_0-9]*/)) {
    h = substr(tmp, RSTART, RLENGTH); dash = (h ~ /^<<-/)
    sub(/^<<-?[ \t]*\\?["\047]?/, "", h); term = h
  }
  split_comment(line)
  exempt = (CMT ~ /#[ \t]*exit-ok:[ \t]*[^ \t].../)
  if (CODE ~ /^[ \t]*set[ \t].*pipefail/) { hasflag = 1 }
  # swallow
  if (match(CODE, /\|\|[ \t]*(true|:|exit 0|return 0)([ \t;)}&|`"\047]|$)/)) {
    pos = RSTART; guard = (substr(CODE, pos, 12) ~ /\|\|[ \t]*(exit|return)/)
    v = verb_before(pos)
    pre = substr(CODE, 1, pos - 1)
    ok = (v == "kill" || v == "rm" || (v == "wait" && pre ~ /(^|[^A-Za-z_])kill[ \t]/) || (guard && (v == "[" || v == "[[" || v == "test")))
    if (!ok && !exempt) report("swallow")
  }
  # forlist: only the word list between `in` and `; do`, not the loop body
  if (match(CODE, /(^|[;&|{(]|[ \t])for[ \t]+[A-Za-z_][A-Za-z_0-9]*[ \t]+in[ \t]/)) {
    fl = substr(CODE, RSTART + RLENGTH)
    sub(/;[ \t]*do([ \t]|$).*/, "", fl); gsub(/\047[^\047]*\047/, "", fl)
    if (fl ~ /(\$\(|`)/ && !exempt) report("forlist")
  }
  # besteffort
  if (besteffort_misuse(CODE) && !exempt) report("besteffort")
  # declsub
  if (declsub(CODE) && !exempt) report("declsub")
  # argsub
  if (argsub(CODE) && !exempt) report("argsub")
  # setargs
  if (CODE ~ /(^|[;&|{(]|[ \t])set[ \t]+--[ \t].*(\$\(|`)/ && !exempt) report("setargs")
  # pipeline: pipefail must already be set above this line
  p = CODE; gsub(/\047[^\047]*\047/, "", p); gsub(/"[^"]*"/, "", p)
  # procsub: `done < <(cmd)`, `diff <(a) <(b)`: the status of cmd never reaches the shell (not even with set -e or pipefail)
  if (!exempt && p ~ /(^|[^$])[<>]\(/) report("procsub")
  if (!hasflag && !exempt && p ~ /[^|]\|[^|&]/ && p !~ /^[ \t]*[^ \t]*\|[^ \t]*\)/) report("pipeline")
}
END { exit (n > 0) }
AWK

cat > "$tmp/rcbranch.awk" <<'AWK'
# One file at a time. rcbranch: `v=$(cmd) || rc=$?` records the status of a command whose OUTPUT is then used (#466). The `default` and `fnctx` rules
# (where a command substitution may stand, what may follow it, what a function body must say) are judged on a syntax tree by tools/swallowast (#472).
{
  line = $0
  if (term != "") { t = line; if (dash) sub(/^\t+/, "", t); if (t == term) term = ""; L[NR] = ""; next }   # heredoc bodies are data
  L[NR] = line
  tmp = line; gsub(/<<</, "", tmp)
  if (line !~ /^[ \t]*#/ && match(tmp, /<<-?[ \t]*\\?["\047]?[A-Za-z_][A-Za-z_0-9]*/)) {
    h = substr(tmp, RSTART, RLENGTH); dash = (h ~ /^<<-/); sub(/^<<-?[ \t]*\\?["\047]?/, "", h); term = h
  }
}
END {
  # rcbranch: `v=$(cmd) || rc=$?` records the status of a command whose OUTPUT is then used. Something must read $rc, before $v is used, and
  # the lines that read it must be able to refuse the output (return / exit / continue / break; a new value for $v does not count).
  # Records-and-ignores (jq failed, "nothing to approve" printed, rows used anyway, #466) is red. Heuristic: it does not prove the branch
  # is right, only that one exists; docs/ci.md lists the sites it covers.
  for (i = 1; i <= NR && FILENAME != "scripts/test.sh"; i++) {   # ONLY the repo's scripts/test.sh (as git ls-files names it) is the harness; a test.sh anywhere else is scanned: its $rc is judged by expect_eq, not by a branch
    r = L[i]; sub(/[ \t]#.*/, "", r)
    if (!match(r, /(^|[;&|{(]|[ \t])[A-Za-z_][A-Za-z_0-9]*=("|\047)?(\$\(|`)/)) continue
    if (r !~ /\|\|[ \t]*[A-Za-z_][A-Za-z_0-9]*=\$\?/) continue
    v = substr(r, RSTART, RLENGTH); sub(/^[;&|{( \t]+/, "", v); sub(/=.*/, "", v)
    c = r; sub(/.*\|\|[ \t]*/, "", c); sub(/=.*/, "", c)
    if (L[i] ~ /#[ \t]*exit-ok:[ \t]*[^ \t].../) continue
    rd = 0; use = 0
    for (k = i + 1; k <= i + 8 && k <= NR; k++) {
      if (!rd && L[k] ~ ("\\$\\{?" c "([^A-Za-z_0-9]|$)")) rd = k
      if (!use && L[k] ~ ("\\$\\{?" v "([^A-Za-z_0-9]|$)")) use = k
    }
    code = L[i]; sub(/^[ \t]+/, "", code)
    if (!rd) { printf "%s:%d: rcbranch: %s   [$%s is never read within 8 lines]\n", FILENAME, i, code, c; n++; continue }
    if (use && use < rd) { printf "%s:%d: rcbranch: %s   [$%s is used at line %d before $%s is read at line %d]\n", FILENAME, i, code, v, use, c, rd; n++; continue }
    esc = 0
    for (k = rd; k <= rd + 5 && k <= NR; k++) {
      if (L[k] ~ /(^|[^A-Za-z_0-9])(continue|break)([^A-Za-z_0-9]|$)/ || L[k] ~ ("(^|[^A-Za-z_0-9])(return|exit)([ \t]+(\"?[1-9][0-9]*\"?|\"?\\$\\?\"?|\"?\\$\\{?" c "\\}?\"?))?[ \t]*(;|\\}|&|\\||\\)|$)")) { esc = 1; break }   # return / exit count only with a provably non-zero status: nothing, a non-zero literal, $?, or the captured $c (not `exit "$zero"`)
    }
    if (!esc) { printf "%s:%d: rcbranch: %s   [the lines that read $%s (from line %d) never refuse $%s: no return / exit / continue / break]\n", FILENAME, i, code, c, rd, v; n++ }
  }
  exit (n > 0)
}
AWK

rc=0; awk -f "$tmp/scan.awk" "${files[@]}" > "$tmp/out" || rc=$?
case $rc in 0 | 1) ;; *) die "awk failed (exit $rc)" ;; esac
for f in "${files[@]}"; do
  rc=0; awk -f "$tmp/rcbranch.awk" "$f" >> "$tmp/out" || rc=$?
  case $rc in 0 | 1) ;; *) die "awk (rcbranch) failed on $f (exit $rc)" ;; esac
done

# `default` and `fnctx` (#472): where a command substitution may stand, judged on a syntax tree by tools/swallowast. The binary is built once into ./bin
# (a fresh checkout builds it on first use; a rebuild happens only when a source file under tools/swallowast is newer than the binary).
ast="$root/bin/swallowast"
command -v go >/dev/null || die "go is not installed: the default / fnctx layer (tools/swallowast) cannot be built"
stale=1
if [ -x "$ast" ]; then
  newer=$(find "$root/tools/swallowast" -type f \( -name '*.go' -o -name go.mod -o -name go.sum \) -newer "$ast" -print) || { echo "check-exit-swallow: find failed" >&2; exit 2; }
  [ -n "$newer" ] || stale=0
fi
if [ "$stale" = 1 ]; then
  mkdir -p "$root/bin" || die "mkdir bin failed"
  rc=0; ( cd "$root/tools/swallowast" && go build -buildvcs=false -o "$ast.$$" . ) > "$tmp/build.out" 2>&1 || rc=$?
  if [ "$rc" != 0 ]; then bm=$(head -5 "$tmp/build.out") || exit 2; echo "$bm" >&2; die "go build tools/swallowast failed (exit $rc)"; fi
  mv -f "$ast.$$" "$ast" || die "mv of the built swallowast failed"
fi
rc=0; "$ast" "${files[@]}" >> "$tmp/out" 2> "$tmp/ast.err" || rc=$?
case $rc in 0 | 1) ;; *) am=$(head -3 "$tmp/ast.err") || exit 2; echo "$am" >&2; die "swallowast failed (exit $rc)" ;; esac

# Second layer, a real shell parser: shellcheck SC2155 ("declare and assign separately to avoid masking return values") sees what a line
# scanner cannot — continuation lines, `a="x"'y z' b=$(cmd)`, `$'..'` values. A file shellcheck cannot parse would be skipped
# silently by `-i SC2155`, so every file must also pass `bash -n`. shellcheck missing is an error, never a skipped check.
command -v shellcheck >/dev/null || { echo "check-exit-swallow: shellcheck is not installed: the SC2155 (declsub) layer cannot run" >&2; exit 2; }
for f in "${files[@]}"; do
  rc=0; bash -n "$f" 2> "$tmp/syntax" || rc=$?
  if [ "$rc" != 0 ]; then sm=$(head -1 "$tmp/syntax") || exit 2; die "$f does not parse (bash -n): $sm"; fi
done
rc=0; shellcheck -s bash -f gcc -i SC2155 -- "${files[@]}" > "$tmp/sc.out" 2> "$tmp/sc.err" || rc=$?
case $rc in 0 | 1) ;; *) sm=$(head -2 "$tmp/sc.err") || exit 2; die "shellcheck failed (exit $rc): $sm" ;; esac
while IFS= read -r row; do
  [ -n "$row" ] || continue
  sf=${row%%:*}; rest=${row#*:}; sl=${rest%%:*}
  src=$(sed -n "${sl}p" "$sf") || { echo "check-exit-swallow: cannot read line $sl of $sf" >&2; exit 2; }
  if printf '%s\n' "$src" | grep -Eq '#[[:space:]]*exit-ok:[[:space:]]*[^[:space:]].{2,}'; then continue; fi
  trimmed=$(printf '%s' "$src" | sed 's/^[[:space:]]*//') || { echo "check-exit-swallow: sed failed" >&2; exit 2; }
  printf '%s:%s: declsub: %s   [shellcheck SC2155]\n' "$sf" "$sl" "$trimmed" >> "$tmp/out"
  rc=1
done < "$tmp/sc.out"

# findings in an extracted script are reported against the YAML file and line
awk -F'\t' -v map="$tmp/map.tsv" 'FILENAME == map { first[$1] = $3; src[$1] = $2; next }
  { i = index($0, ":"); f = substr($0, 1, i - 1)
    if (f in src) { rest = substr($0, i + 1); j = index(rest, ":"); k = substr(rest, 1, j - 1)
      print src[f] ":" (first[f] + k - 2) ":" substr(rest, j + 1) } else print }' "$tmp/map.tsv" "$tmp/out" > "$tmp/out.mapped" || { echo "check-exit-swallow: mapping findings failed" >&2; exit 2; }
mv "$tmp/out.mapped" "$tmp/out" || { echo "check-exit-swallow: mv failed" >&2; exit 2; }
if [ -s "$tmp/out" ]; then
  cat "$tmp/out" >&2
  n_findings=$(wc -l < "$tmp/out" | tr -d ' ') || { echo "check-exit-swallow: wc failed" >&2; exit 2; }
  echo "check-exit-swallow: $n_findings finding(s). Fix the line, or for rules that allow it add '# exit-ok: <why the status cannot change the verdict>' (docs/ci.md). default and fnctx findings CANNOT be exempted: after \`x=\$(…) ||\` only \`return\`/\`exit\` (bare or \`\$?\` straight after the \`||\`), \`return N\`/\`exit N\` or \`{ …; return N; }\`/\`{ …; exit N; }\` with N a literal 1..255 (no \$var, no \`\$?\` after another command); inside \$(…) only an \`&&\` chain of pipelines (no ; newline & !), optionally with \`|| exit [N]\`, or the label \`[ test ] && echo a || echo b\`." >&2
  exit 1
fi
echo "check-exit-swallow: $nscripts scripts and the run: scripts of $nyml workflow / action files, no finding for the covered shapes (heuristic; what it does not follow: docs/ci.md 〈不透明範圍〉, #472)"
