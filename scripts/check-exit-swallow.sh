#!/usr/bin/env bash
# Static gate for #462: no shell script in the repo may throw away an external command's exit status without saying why.
# A gate that swallows a status is green when the command it asked about failed (#453/#455, #457 r0, #453 r1, #458).
#
#   scripts/check-exit-swallow.sh              # every tracked *.sh except scripts/hooks/ (task check-exit-swallow)
#   scripts/check-exit-swallow.sh <file>...    # only these files (scripts/test.sh's fixtures); no scripts/hooks/ skip here, and a file inside the repo is
#                                              # named by its repo-relative path however it was spelled (./x, ../x, absolute)
#
# It flags, per line (comment lines and heredoc bodies are skipped):
#   swallow   `cmd || true`, `cmd || :`, `cmd || exit 0`, `cmd || return 0` — also inside $(...), a trap string or sh -c '...'
#   setargs   `set -- $(cmd)`: same, the status of cmd is dropped
#   besteffort best_effort inside $(…) or on the right of an assignment: it hides the status of a command whose output is used
#   declsub   `local n="$(cmd)"`, `export X=$(cmd)`, `declare`/`readonly`: the declaration's own status hides cmd's; split into `local n; n=$(cmd)`
#   argsub    `expect_eq x "$(… | grep -c y)"`, `[ "$(grep -c y f)" -ge 2 ]`: a match count inside an argument; assign it first
#   forlist   `for x in $(cmd)` / `for x in `cmd``: the status of cmd is dropped (a short or failed `seq` is a shorter loop)
#   pipeline  `a | b` in a file that never says `set -o pipefail` (or set -euo pipefail ...): only b's status counts
#   procsub   `done < <(cmd)`, `diff <(a) <(b)`: the status of a process substitution is never seen, even under set -e / pipefail
#   rcbranch  `v=$(cmd) || rc=$?` whose output $v is used: $rc must be read (before $v is) by lines that can refuse it (return / exit / continue /
#             break; no function names, and a new value for $v is the failure swallowed; top-level code only: inside a function the fnctx shapes apply). Heuristic: it checks that a refusing branch EXISTS, not that it is right. Only scripts/test.sh itself is exempt (a test.sh elsewhere is scanned).
#   default   (#470) a failed command turned into a value. Two kinds are flagged: (a) `x=$(…) || H` where H is outside the hshape() whitelist (nothing or
#             `$?` straight after the `||`; otherwise a literal 1..255; inside braces only a literal 1..255); (b) a `$( … )` whose content is not ONE `&&` chain
#             of simple pipelines (no `;`, second line, `&`, `!`, subshell, group), optionally followed by one non-zero `|| exit` handler, or the label
#             `$([ test ] && echo a || echo b)`. Not exemptible.
#             KNOWN GAPS, not detected (tracked in #472, parser rewrite): `$(…)` in an argument (`echo "$(cmd)"`, `f "$(cmd)"`), a nested substitution
#             (`$(echo "$(cmd)")`), `$(…)` inside a heredoc or here-string, the dynamic content of eval / . / source.
#   fnctx     inside ANY function body, `x=$(cmd)` / `x="$(cmd)"` must be followed on its own line (or after a heredoc substitution's closing line) by
#             one of THREE shapes: `|| return` / `|| exit` (bare, or with `$?` straight after the `||`), `|| return N` / `|| exit N` (N a literal 1..255), or
#             `|| { …; return N; }` / `{ …; exit N; }` (the group's LAST statement, N a literal 1..255; no bare, no `$?`, no `$var`). Nothing else:
#             no function names, no new value, no `|| rc=$?`, no `$?` on a later line, no continue / break, no return 0. A shape rule: it does not look at how or from where the function is called (`f || x`,
#             `if f`, `$(f)`, a wrapper, eval, command f, another file, never) - errexit may be off in any of those and the checker is never
#             one spelling behind that way.
#   Workflow / composite-action files: the `run:` scripts are cut out (scripts/extract-run-blocks.py) and scanned with these same rules;
#             a step with no `shell:` runs `bash -e {0}` (no pipefail), `shell: bash` runs with pipefail, so the first is held to `pipeline`.
# Not flagged without a comment: `kill ... || true`, `rm ... || true` and `... kill ... && wait ... || true` (cleanup:
# an already-gone process or file is not a verdict). Anything else needs either a fix or, on the SAME line, a comment
#   # exit-ok: <why this command's status cannot change the verdict>
# The fixes: at top level `n=$(grep -c x f || [ $? -eq 1 ])` (grep: 1 = no match, 2 = error stays fatal) or an explicit `rc=0; cmd || rc=$?`
# followed by a case on $rc; inside a function only the three fnctx shapes, with a tolerant exit status in a helper (le1: `n=$(le1 grep -c x f) || return`);
# `for ((i = 0; i < N; i++))` instead of $(seq). docs/ci.md has the rules and the reasons.
#
# This is a line scanner, not a shell parser: `pipeline` only looks at files without pipefail, and a `|` inside a quoted
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
  [ "$rc" = 0 ] || die "extract-run-blocks.py failed (exit $rc): $(head -2 "$tmp/extract.err")"
  while IFS=$'\t' read -r sh _ _; do files+=("$sh"); done < "$tmp/map.tsv"
fi

# nothing to scan (a workflow whose only run: steps are not shell, e.g. `shell: python`): clean, and awk / shellcheck must not be started with no file
if [ "${#files[@]}" -eq 0 ]; then echo "check-exit-swallow: no shell to check in the ${nyml} workflow / action file(s)"; exit 0; fi

cat > "$tmp/common.awk" <<'AWK'
# The ONE judgment of what may follow `||` after a failing command (#466 fnctx, #470 default, rcbranch). A closed whitelist INCLUDING the argument: only what is
# provably a non-zero status. A status taken from a variable can be 0 (`exit "$zero"`), and `$?` after another command is THAT command's status
# (`{ echo 0; exit $?; }` and a bare `{ echo 0; exit; }` leave with 0).
#   plain   (directly after `||`)  return | exit | return $? | exit $? | return N | exit N     here `$?` IS the failed command's status; N = non-zero integer literal
#   brace   { …; return N; }  { …; exit N; }                                                    the LAST statement, N a non-zero integer literal. No bare, no `$?`, no `$var`.
# hshape(h, exit_only): 1 when the handler text h (what follows `||`, a brace group possibly joined from several lines with " ; ") is one of them.
# exit_only: inside a $(…) the handler may only `exit` (the status of the substitution reaches the assignment), never `return`.
function hshape(h, exit_only,   hs, blk, ns, st, i, verb, lit, last) {
  verb = exit_only ? "exit" : "(return|exit)"
  lit = "\"?([1-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])\"?"             # 1..255: an exit status is taken modulo 256, so 256 and 512 are 0
  sub(/^[ \t]+/, "", h)
  if (h ~ /^\{/) {
    if (index(h, "}") == 0) return 0
    blk = substr(h, 1, index(h, "}") - 1); sub(/^\{[ \t]*/, "", blk); sub(/[ \t;]+$/, "", blk)
    if (blk ~ /[{}]/) return 0                                       # nested groups: not judged, so not accepted
    ns = split(blk, st, ";"); last = st[ns]; sub(/^[ \t]+/, "", last); sub(/[ \t]+$/, "", last)
    return (last ~ ("^" verb "[ \t]+" lit "$"))
  }
  hs = h; sub(/[ \t]*(;|&&|\|\|).*/, "", hs)                        # a plain shape ends at the first separator
  return (hs ~ ("^" verb "([ \t]+(\\$\\?|\"\\$\\?\"|" lit "))?$"))
}
AWK
cat > "$tmp/scan.awk" <<'AWK'
# Reads the files named on the command line. Prints "file:line: rule: code" per finding; exit 1 if there was one.
function reset_file() { term = ""; dash = 0; hasflag = 0; sd = 0; dq = 0; sq = 0; aq = 0; ah_open = 0; ah_wait = 0 }
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
# default (#470): a failed command turned into a value. A CLOSED whitelist, not a list of bad words (a keyword list is what #466 r6 and #470 r0 got
# bypassed through: `{ echo 0; }`, `(echo 0)`, `{ x=0; }`). Walks CODE character by character with a stack of open $(…) / `…` / (…) frames that
# survives line breaks, and judges every handler after a `||` with hshape() (common.awk, the same judgment fnctx uses):
#   x=$(…) || H            H = hshape(): return|exit (bare or `$?` right after the `||`), return|exit N, { …; return|exit N; }, N = 1..255
#                          (or `|| NAME=$?`: the status is kept, rcbranch checks the branch)
#   $(… || H …)            H = the same, exit only, and nothing may follow it
#   label idiom            $([ test ] && echo a || echo b): the left side is `[` / `[[` / `test` with NO $( ` <( inside it (a command inside the test
#                          would be a failure turned into a label). A function or external command on the left is not a label: yesno() it.
# Anything else after a `||` there is a finding. Not exemptible. Limits: a `case` pattern `a)` inside a $( … ) pops a frame early; the handler of an
# assignment is read for at most 6 lines.
function reportat(rule, ln, txt) { printf "%s:%d: %s: %s\n", FILENAME, ln, rule, txt; n++ }
function defpush(t, nm, need) { sd++; ft[sd] = t; fb[sd] = ""; fn[sd] = nm; fneed[sd] = need; fln[sd] = FNR; flt[sd] = line; fdq[sd] = dq; fbd[sd] = 0; if (t != "p") dq = 0 }   # a substitution is a fresh quoting context
# What may stand inside $( … ): a CLOSED whitelist, not a list of bad constructs.
#   1. ONE `&&` chain of pipelines of simple commands: no `;`, no second line, no `&` (background), no `!` on any segment, no ( … ) subshell, no { … } group  (a later statement or a negation hides the failure of the first:
#      `$(cmd; echo 0)`, `$(cmd⏎echo 0)`, `$(cmd; true)`, `$(cmd & wait)`, `$(! cmd)`, `$(a && ! b)`, `$(set +e; cmd; echo 0)`). Pipelines joined by `&&` are fine:
#      the failure of the first is the status of the chain (`$(cd d && pwd)`)
#   2. that pipeline followed by `|| H`, H = hshape(exit only)
#   3. the label `[ test ] && echo a || echo b` (test = `[` / `[[` / `test`, no $( ` <( inside)
# Everything else is a finding; `if` / `case` / `while` / `for` need `;` or a newline, so they fall outside without being listed.
function labelok(prev, h,   lc, cond) {            # `[ test ] && echo a || echo b`: prev = the clause before `||`, h = the handler
  lc = prev; sub(/.*;/, "", lc); cond = lc; sub(/&&[ \t]*(echo|printf).*$/, "", cond)
  return (h ~ /^(echo|printf)([ \t;)`"\047]|$)/ && lc ~ /&&[ \t]*(echo|printf)/ && cond ~ /^[ \t]*(\[\[?|test)[ \t]/ && cond !~ /(\$\(|`|<\()/)
}
function onepipeline(t,   x) {                       # t: one part of a substitution, quotes already removed
  x = t
  gsub(/[ \t]*\002[ \t]*$/, "", x); gsub(/^[ \t\002]+/, "", x)                    # blank lines around it are nothing
  gsub(/(\|\||\|)[ \t]*\002[ \t]*/, "| ", x)                                      # a pipeline continued on the next line
  gsub(/[0-9]*[<>]&[0-9-]*/, "", x); gsub(/&>>?/, "", x)                          # 2>&1, >&2, &> are redirections, not `&`
  if (x ~ /[;\002]/) return 0                                                      # a second statement
  gsub(/&&[ \t]*/, "\003", x)                                                      # `a && b`: the failure of a is the status of the whole chain, no default is made
  if (x ~ /&/) return 0                                                            # `&` background
  if (x ~ /\(P\)/) return 0                                                       # a ( … ) subshell: whatever runs in it can hide a failure (`$(true && (cmd; echo 0))`, `$( (cmd) )`)
  if (x ~ /(^|[ \t;|\003])\{[ \t]/) return 0                                       # a { … } group, likewise (the handler's `|| { …; exit N; }` is judged by hshape, not here)
  if (x ~ /(^|\003)[ \t]*!/) return 0                                              # a negated pipeline, anywhere in the chain (`a && ! b`)
  return 1
}
function hexact(h,   r) {                          # inside $( … ) the handler is the END of the substitution: nothing may follow it (`|| { exit 3; }; echo 0`)
  if (h ~ /^\{/) { r = substr(h, index(h, "}") + 1); return (r ~ /^[ \t\002;]*$/) }
  return (h !~ /(;|&&|\|\|)/ && h !~ /[^ \t\002].*\002[ \t]*[^ \t\002]/)
}
function defeval(d,   np, parts, oparts, k, h, plain, p1, op1) {
  plain = fb[d]; gsub(/"[^"]*"/, "", plain); gsub(/\047[^\047]*\047/, "", plain)         # for the STRUCTURE (; & ! newline): text inside quotes is text
  np = split(plain, parts, "\001"); split(fb[d], oparts, "\001")                          # the handlers and the label test are judged on the original text: `exit "$zero"`, `[ -f "$(cmd)" ]`
  p1 = parts[1]; op1 = oparts[1]
  h = oparts[2]; sub(/^[ \t\002]+/, "", h)
  if (np == 2 && labelok(op1, h) && p1 !~ /;/) return                                     # the label idiom
  if (!onepipeline(p1)) { reportat("default", fln[d], flt[d]); return }
  for (k = 2; k <= np; k++) {
    h = oparts[k]; gsub(/^[ \t\002]+/, "", h); gsub(/[ \t\002]+$/, "", h)
    if (!hshape(h, 1) || !hexact(h)) { reportat("default", fln[d], flt[d]); return }
  }
}
# cut a trailing `# comment` off a short piece of code, honoring quotes (split_comment loses the comment of a line that ends a multi-line string)
function cutcomment(t,   i, ch, q, out) {
  q = ""
  for (i = 1; i <= length(t); i++) {
    ch = substr(t, i, 1)
    if (q == "" && ch == "#" && (i == 1 || substr(t, i - 1, 1) ~ /[ \t;]/)) return substr(t, 1, i - 1)
    if (ch == "\\" && q != "\047") { i++; continue }
    if (q == "" && (ch == "\"" || ch == "\047")) q = ch; else if (q == ch) q = ""
  }
  return t
}
function ahjudge(   b) {
  b = cutcomment(ah_buf); sub(/^[ \t;]+/, "", b); sub(/[ \t]+$/, "", b)
  if (b == "" || (b ~ /^\{/ && index(b, "}") == 0)) { if (ah_cnt >= 6) { reportat("default", ah_ln, ah_txt); ah_open = 0 }; return }
  if (!(hshape(b, 0) || b ~ /^[A-Za-z_][A-Za-z_0-9]*=\$\?[ \t]*(;|$)/)) reportat("default", ah_ln, ah_txt)
  ah_open = 0
}
function defscan(c,   i, n, ch, nx, rest, nm, pre, rem, t, d, prevc) {
  n = length(c)
  if (c ~ /[^ \t]/) {                                       # the first non-blank line after one that ended in `)` or `||`: the handler of an assignment may be here
    if (ah_wait) { ah_wait = 0; if (c ~ /^[ \t]*\|\|/) { ah_buf = c; sub(/^[ \t]*\|\|[ \t]*/, "", ah_buf); ah_open = 1; ah_cnt = 0; ahjudge() } }
    else if (ah_open) { t = c; sub(/^[ \t]+/, "", t); ah_buf = ah_buf " ; " t; ah_cnt++; ahjudge() }
  }
  for (i = 1; i <= n; i++) {
    ch = substr(c, i, 1); nx = substr(c, i + 1, 1)
    if (aq) { if (ch == "\\") { if (sd > 0) fb[sd] = fb[sd] ch nx; i++; continue }; if (ch == "\047") aq = 0; if (sd > 0) fb[sd] = fb[sd] ch; continue }   # $'…': a backslash escapes the next character, even a quote
    if (sq) { if (ch == "\047") sq = 0; if (sd > 0) fb[sd] = fb[sd] ch; continue }
    if (ch == "#" && !dq && (i == 1 || substr(c, i - 1, 1) ~ /[ \t;]/)) break                      # a comment: backticks and `||` in it are text
    if (ch == "$" && nx == "\047" && !dq) { aq = 1; if (sd > 0) fb[sd] = fb[sd] ch nx; i++; continue }
    if (ch == "\\") { if (sd > 0) fb[sd] = fb[sd] ch nx; i++; continue }
    if (ch == "\047" && !dq) { sq = 1; if (sd > 0) fb[sd] = fb[sd] ch; continue }
    if (ch == "\"") { dq = !dq; if (sd > 0) fb[sd] = fb[sd] ch; continue }
    if (ch == "$" && nx == "(") {
      if (substr(c, i + 2, 1) == "(") { defpush("a", "", 2); i += 2; continue }
      pre = substr(c, 1, i - 1); nm = ""
      if (match(pre, /[A-Za-z_][A-Za-z_0-9]*=("|\047)?$/)) { nm = substr(pre, RSTART, RLENGTH); sub(/=.*/, "", nm) }
      defpush("s", nm, 1); i++; continue
    }
    if (ch == "`") {
      if (sd > 0 && ft[sd] == "b") { defeval(sd); dq = fdq[sd]; sd--; if (sd > 0) fb[sd] = fb[sd] "$(S)" }
      else { pre = substr(c, 1, i - 1); nm = ""; if (match(pre, /[A-Za-z_][A-Za-z_0-9]*=("|\047)?$/)) { nm = substr(pre, RSTART, RLENGTH); sub(/=.*/, "", nm) }; defpush("b", nm, 1) }
      continue
    }
    if (!dq && ch == "(") { defpush("p", "", 1); continue }
    if (ch == ")" && (!dq || (sd > 0 && ft[sd] != "p"))) {
      if (sd == 0) continue
      fneed[sd]--
      if (fneed[sd] > 0) continue
      nm = fn[sd]; t = ft[sd]; d = sd
      if (t == "s" || t == "b") defeval(d)
      dq = fdq[d]; sd--
      if (sd > 0) fb[sd] = fb[sd] (t == "p" ? "(P)" : (t == "a" ? "$((A))" : "$(S)"))
      if (t == "s" && nm != "") {                           # x=$(…) closed: what follows the `)` is the handler
        rem = substr(c, i + 1); sub(/^"/, "", rem)
        if (rem ~ /^[ \t]*\|\|/) { ah_buf = rem; sub(/^[ \t]*\|\|[ \t]*/, "", ah_buf); ah_open = 1; ah_cnt = 0; ah_ln = fln[d]; ah_txt = flt[d]; ahjudge() }
        else if (rem ~ /^[ \t]*$/) { ah_wait = 1; ah_ln = fln[d]; ah_txt = flt[d] }
      }
      continue
    }
    if (!dq && ch == "|" && nx == "|" && sd > 0 && (ft[sd] == "s" || ft[sd] == "b")) {
      if (fbd[sd] == 0) fb[sd] = fb[sd] "\001"; else fb[sd] = fb[sd] "||"
      i++; continue
    }
    if (sd > 0) {
      prevc = substr(fb[sd], length(fb[sd]), 1)
      if (!dq && ch == "{" && (nx == " " || nx == "" || nx == "\t")) fbd[sd]++
      else if (!dq && ch == "}" && fbd[sd] > 0 && (prevc == " " || prevc == ";" || prevc == "\t" || prevc == "")) fbd[sd]--
      fb[sd] = fb[sd] ch
    }
  }
  if (sd > 0 && c !~ /\\[ \t]*$/) for (i = 1; i <= sd; i++) fb[i] = fb[i] "\002"    # a line break inside a substitution separates statements (a trailing backslash does not)
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
  defscan(CODE)
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

cat > "$tmp/fnctx.awk" <<'AWK'
# One file at a time. A SHAPE rule, not a call-graph one: inside ANY function body, every `x=$(cmd)` / `x="$(cmd)"` must say what happens when
# cmd fails (`|| …` on the line, `$?` read right after, or the `|| …` after a heredoc substitution's closing line). Where the function is
# called from (`f || x`, `if f`, `$(f)`, a wrapper, eval, command f, another file) decides whether errexit protects it, and a checker that
# reads call sites is always one spelling behind; the body is the only place that is the same everywhere.
function strip(s,   t) { t = s; gsub(/\047[^\047]*\047/, "", t); gsub(/"[^"]*"/, "", t); sub(/[ \t]#.*/, "", t); return t }
{
  line = $0
  if (term != "") { t = line; if (dash) sub(/^\t+/, "", t); if (t == term) { term = ""; ENDHD[NR] = 1 }; L[NR] = ""; T[NR] = ""; next }   # heredoc bodies are data (ENDHD: the terminator line)
  L[NR] = line; T[NR] = strip(line)
  tmp = line; gsub(/<<</, "", tmp)
  if (line !~ /^[ \t]*#/ && match(tmp, /<<-?[ \t]*\\?["\047]?[A-Za-z_][A-Za-z_0-9]*/)) {
    h = substr(tmp, RSTART, RLENGTH); dash = (h ~ /^<<-/); sub(/^<<-?[ \t]*\\?["\047]?/, "", h); term = h
  }
}
END {
  nf = 0
  for (i = 1; i <= NR; i++) {
    if (match(T[i], /^[ \t]*(function[ \t]+)?[A-Za-z_][A-Za-z_0-9]*[ \t]*(\(\))?[ \t]*\{/) && (T[i] ~ /\(\)/ || T[i] ~ /^[ \t]*function[ \t]/)) {
      d = T[i]; sub(/^[ \t]*(function[ \t]+)?/, "", d); sub(/[ \t]*(\(\))?[ \t]*\{.*/, "", d)
      ind = L[i]; sub(/[^ \t].*/, "", ind)
      e = i; tq = L[i]; openq = (gsub(/\047/, "", tq) % 2 == 1)   # the definition line leaves a single quote open: the body's `; }` is on a later line
      if (T[i] !~ /\}[ \t]*(;|$|[|&])/ || T[i] ~ /\{[ \t]*$/) { for (e = i + 1; e <= NR; e++) if ((index(L[e], ind "}") == 1 && L[e] ~ /^[ \t]*\}/) || (openq && L[e] ~ /;[ \t]*\}[ \t]*(#.*)?$/)) break }
      nf++; name[nf] = d; from[nf] = i; to[nf] = e
    }
  }
  for (f = 1; f <= nf; f++) {
    for (i = from[f]; i <= to[f]; i++) {
      t = L[i]; sub(/[ \t]#.*/, "", t); gsub(/\$\(\(/, "", t)
      gsub(/(local|declare|typeset|export|readonly)[ \t]+(-[A-Za-z]+[ \t]+)*[A-Za-z_][A-Za-z_0-9]*(\[[^]]*\])?=("|\047)?(\$\(|`)/, "", t)   # declaration forms are declsub's rule (and SC2155's)             # the RAW line (a quoted `n="$(cmd)"` counts); $(( … )) is arithmetic
      if (i > from[f] && L[i-1] ~ /\\[ \t]*$/ && L[i-1] ~ /(^|[ \t;{])(local|declare|typeset|export|readonly)[ \t]/) continue   # continuation of a declaration: SC2155's
      if (t !~ /(^|[;&|{(]|[ \t])[A-Za-z_][A-Za-z_0-9]*(\[[^]]*\])?\+?=("|\047)?(\$\(|`)/) continue
      if (t ~ /(&&|\|\||(^|[;{(]|[ \t])(if|elif|while|until|!))[ \t]*[^;]*[A-Za-z_][A-Za-z_0-9]*\+?=("|\047)?(\$\(|`)/) continue   # the assignment IS the condition: its status is read
      if (t ~ /^[ \t]*(local|declare|typeset|export|readonly)[ \t]/) continue    # declsub's job
      code = L[i]; sub(/^[ \t]+/, "", code)                                   # no exit-ok here: a function body has no legitimate exemption
      vn = ""; if (match(t, /[A-Za-z_][A-Za-z_0-9]*(\[[^]]*\])?\+?=("|\047)?(\$\(|`)/)) { vn = substr(t, RSTART, RLENGTH); sub(/[^A-Za-z_0-9].*/, "", vn) }
      ok = 0
      hb = i                                                                 # a heredoc inside the substitution: the `)` comes after its terminator
      if (match(L[i], /<<-?[ \t]*\\?["\047]?[A-Za-z_][A-Za-z_0-9]*/)) {
        for (k = i + 1; k <= to[f]; k++) if (ENDHD[k]) { hb = k; break }
      }
      # 1. `|| <handler>` after the substitution's `)` (same line, or the next line starts with it). CLOSED whitelist, three shapes only:
      #      x=$(…) || return|exit [$?|N]      x=$(…) || { …; return|exit N; }      (N a literal 1..255; hshape() in common.awk is the one judgment)
      #    The brace group is parsed: its LAST top-level statement must be that return / exit with a literal. No function names (die, fail), no new value
      #    for the variable, no `|| rc=$?`, no `$?` read on a later line, no continue / break, no $var: the status has to be refused right where it is captured.
      for (k = i; k <= (hb > i ? hb + 2 : i + 3) && k <= to[f] && !ok; k++) {
        hnd = ""
        if (match(L[k], /\)["\047]?[ \t]*\|\|[ \t]*/)) hnd = substr(L[k], RSTART + RLENGTH)
        else if (k > i && match(L[k], /^[ \t]*\|\|[ \t]*/)) hnd = substr(L[k], RSTART + RLENGTH)
        else continue
        sub(/[ \t]#.*/, "", hnd)
        hk = k
        if (hnd ~ /^[ \t]*$/ && k < NR) { hk = k + 1; hnd = L[hk]; sub(/[ \t]#.*/, "", hnd) }   # `|| ` at the end of the line, the handler on the next one
        sub(/^[ \t]+/, "", hnd); sub(/[ \t;]+$/, "", hnd)
        blk = hnd; bm = hk
        if (hnd ~ /^\{/) while (blk !~ /\}$/ && bm < hk + 5 && bm < to[f]) { bm++; t2 = L[bm]; sub(/[ \t]#.*/, "", t2); sub(/^[ \t]+/, "", t2); sub(/[ \t;]+$/, "", t2); blk = blk " ; " t2 }
        if (hshape(blk, 0)) ok = 1
      }
      if (ok) continue
      printf "%s:%d: fnctx: %s   [in %s(): a function body must check this substitution's status itself — errexit may be off wherever the function is called]\n", FILENAME, i, code, name[f]
      n++
    }
  }
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

rc=0; awk -f "$tmp/common.awk" -f "$tmp/scan.awk" "${files[@]}" > "$tmp/out" || rc=$?
case $rc in 0 | 1) ;; *) die "awk failed (exit $rc)" ;; esac
for f in "${files[@]}"; do
  rc=0; awk -f "$tmp/common.awk" -f "$tmp/fnctx.awk" "$f" >> "$tmp/out" || rc=$?
  case $rc in 0 | 1) ;; *) die "awk (fnctx) failed on $f (exit $rc)" ;; esac
done

# Second layer, a real shell parser: shellcheck SC2155 ("declare and assign separately to avoid masking return values") sees what a line
# scanner cannot — continuation lines, `a="x"'y z' b=$(cmd)`, `$'..'` values. A file shellcheck cannot parse would be skipped
# silently by `-i SC2155`, so every file must also pass `bash -n`. shellcheck missing is an error, never a skipped check.
command -v shellcheck >/dev/null || { echo "check-exit-swallow: shellcheck is not installed: the SC2155 (declsub) layer cannot run" >&2; exit 2; }
for f in "${files[@]}"; do bash -n "$f" 2> "$tmp/syntax" || die "$f does not parse (bash -n): $(head -1 "$tmp/syntax")"; done
rc=0; shellcheck -s bash -f gcc -i SC2155 -- "${files[@]}" > "$tmp/sc.out" 2> "$tmp/sc.err" || rc=$?
case $rc in 0 | 1) ;; *) die "shellcheck failed (exit $rc): $(head -2 "$tmp/sc.err")" ;; esac
while IFS= read -r row; do
  [ -n "$row" ] || continue
  sf=${row%%:*}; rest=${row#*:}; sl=${rest%%:*}
  src=$(sed -n "${sl}p" "$sf") || { echo "check-exit-swallow: cannot read line $sl of $sf" >&2; exit 2; }
  if printf '%s\n' "$src" | grep -Eq '#[[:space:]]*exit-ok:[[:space:]]*[^[:space:]].{2,}'; then continue; fi
  printf '%s:%s: declsub: %s   [shellcheck SC2155]\n' "$sf" "$sl" "$(printf '%s' "$src" | sed 's/^[[:space:]]*//')" >> "$tmp/out"
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
  echo "check-exit-swallow: $(wc -l < "$tmp/out" | tr -d ' ') finding(s). Fix the line, or for rules that allow it add '# exit-ok: <why the status cannot change the verdict>' (docs/ci.md). default and fnctx findings CANNOT be exempted: after \`x=\$(…) ||\` only \`return\`/\`exit\` (bare or \`\$?\` straight after the \`||\`), \`return N\`/\`exit N\` or \`{ …; return N; }\`/\`{ …; exit N; }\` with N a literal 1..255 (no \$var, no \`\$?\` after another command); inside \$(…) only an \`&&\` chain of pipelines (no ; newline & !), optionally with \`|| exit [N]\`, or the label \`[ test ] && echo a || echo b\`." >&2
  exit 1
fi
echo "check-exit-swallow: $nscripts scripts and the run: scripts of $nyml workflow / action files, no swallowed exit status"
