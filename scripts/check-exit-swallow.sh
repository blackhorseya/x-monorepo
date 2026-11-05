#!/usr/bin/env bash
# Static gate for #462: no shell script in the repo may throw away an external command's exit status without saying why.
# A gate that swallows a status is green when the command it asked about failed (#453/#455, #457 r0, #453 r1, #458).
#
#   scripts/check-exit-swallow.sh              # every tracked *.sh except scripts/hooks/ (task check-exit-swallow)
#   scripts/check-exit-swallow.sh <file>...    # only these files (scripts/test.sh's fixtures)
#
# It flags, per line (comment lines and heredoc bodies are skipped):
#   swallow   `cmd || true`, `cmd || :`, `cmd || exit 0`, `cmd || return 0` — also inside $(...), a trap string or sh -c '...'
#   setargs   `set -- $(cmd)`: same, the status of cmd is dropped
#   besteffort best_effort inside $(…) or on the right of an assignment: it hides the status of a command whose output is used
#   declsub   `local n="$(cmd)"`, `export X=$(cmd)`, `declare`/`readonly`: the declaration's own status hides cmd's; split into `local n; n=$(cmd)`
#   argsub    `expect_eq x "$(… | grep -c y)"`, `[ "$(grep -c y f)" -ge 2 ]`: a match count inside an argument; assign it first
#   forlist   `for x in $(cmd)` / `for x in `cmd``: the status of cmd is dropped (a short or failed `seq` is a shorter loop)
#   pipeline  `a | b` in a file that never says `set -o pipefail` (or set -euo pipefail ...): only b's status counts
# Not flagged without a comment: `kill ... || true`, `rm ... || true` and `... kill ... && wait ... || true` (cleanup:
# an already-gone process or file is not a verdict). Anything else needs either a fix or, on the SAME line, a comment
#   # exit-ok: <why this command's status cannot change the verdict>
# The fixes: `n=$(grep -c x f || [ $? -eq 1 ])` (grep: 1 = no match, 2 = error stays fatal), an explicit `rc=0; cmd || rc=$?`
# followed by a case on $rc, `for ((i = 0; i < N; i++))` instead of $(seq). docs/ci.md has the rules and the reasons.
#
# This is a line scanner, not a shell parser: `pipeline` only looks at files without pipefail, and a `|` inside a quoted
# string is ignored. Exit 0 = clean, 1 = findings, 2 = the checker itself failed (never reported as clean).
set -euo pipefail
die() { echo "check-exit-swallow: $*" >&2; exit 2; }

here=$(dirname "$0") || die "dirname failed"
cd "$here/.." || die "cannot cd to the repo root from [$here]"
tmp=$(mktemp -d) || die "mktemp failed"
trap 'rm -rf "$tmp"' EXIT

files=()
if [ "$#" -gt 0 ]; then
  files=("$@")
else
  rc=0; git ls-files -- '*.sh' > "$tmp/ls" || rc=$?
  [ "$rc" = 0 ] || die "git ls-files failed (exit $rc)"
  while IFS= read -r f; do
    case $f in scripts/hooks/*) continue ;; esac
    files+=("$f")
  done < "$tmp/ls"
  [ "${#files[@]}" -ge 20 ] || die "only ${#files[@]} scripts to check — git ls-files '*.sh' no longer returns what it used to"
fi

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
  if (!hasflag && !exempt && p ~ /[^|]\|[^|&]/ && p !~ /^[ \t]*[^ \t]*\|[^ \t]*\)/) report("pipeline")
}
END { exit (n > 0) }
AWK

rc=0; awk -f "$tmp/scan.awk" "${files[@]}" > "$tmp/out" || rc=$?
case $rc in 0 | 1) ;; *) die "awk failed (exit $rc)" ;; esac

# Second layer, a real shell parser: shellcheck SC2155 ("declare and assign separately to avoid masking return values") sees what a line
# scanner cannot — continuation lines, `a="x"'y z' b=$(cmd)`, `$'..'` values. A file shellcheck cannot parse would be skipped
# silently by `-i SC2155`, so every file must also pass `bash -n`. shellcheck missing is an error, never a skipped check.
command -v shellcheck >/dev/null || die "shellcheck is not installed: the SC2155 (declsub) layer cannot run"
for f in "${files[@]}"; do bash -n "$f" 2> "$tmp/syntax" || die "$f does not parse (bash -n): $(head -1 "$tmp/syntax")"; done
rc=0; shellcheck -s bash -f gcc -i SC2155 -- "${files[@]}" > "$tmp/sc.out" 2> "$tmp/sc.err" || rc=$?
case $rc in 0 | 1) ;; *) die "shellcheck failed (exit $rc): $(head -2 "$tmp/sc.err")" ;; esac
while IFS= read -r row; do
  [ -n "$row" ] || continue
  sf=${row%%:*}; rest=${row#*:}; sl=${rest%%:*}
  src=$(sed -n "${sl}p" "$sf") || die "cannot read line $sl of $sf"
  if printf '%s\n' "$src" | grep -Eq '#[[:space:]]*exit-ok:[[:space:]]*[^[:space:]].{2,}'; then continue; fi
  printf '%s:%s: declsub: %s   [shellcheck SC2155]\n' "$sf" "$sl" "$(printf '%s' "$src" | sed 's/^[[:space:]]*//')" >> "$tmp/out"
  rc=1
done < "$tmp/sc.out"

if [ -s "$tmp/out" ]; then
  cat "$tmp/out" >&2
  echo "check-exit-swallow: $(wc -l < "$tmp/out" | tr -d ' ') finding(s). Fix the line or add '# exit-ok: <why the status cannot change the verdict>' on it (docs/ci.md)." >&2
  exit 1
fi
echo "check-exit-swallow: ${#files[@]} scripts, no swallowed exit status"
