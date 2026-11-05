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
#   procsub   `done < <(cmd)`, `diff <(a) <(b)`: the status of a process substitution is never seen, even under set -e / pipefail
#   fnctx     a function that is CALLED in a `f || x` / `f && x` / `if f` / `while f` / `! f` position (errexit is off for its whole body)
#             and assigns a command substitution (`x=$(cmd)`) without a status check on that line. Only call sites in the SAME file
#             are seen (a function sourced from another file and called conditionally here is not), and calls through variables or
#             `$(f)` are not either.
#   Workflow / composite-action files: the `run:` scripts are cut out (scripts/extract-run-blocks.py) and scanned with these same rules;
#             a step with no `shell:` runs `bash -e {0}` (no pipefail), `shell: bash` runs with pipefail, so the first is held to `pipeline`.
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

files=() yml=()
if [ "$#" -gt 0 ]; then
  for f in "$@"; do case $f in *.yml | *.yaml) yml+=("$f") ;; *) files+=("$f") ;; esac; done
else
  rc=0; git ls-files -- '*.sh' > "$tmp/ls" || rc=$?
  [ "$rc" = 0 ] || die "git ls-files failed (exit $rc)"
  while IFS= read -r f; do
    case $f in scripts/hooks/*) continue ;; esac
    files+=("$f")
  done < "$tmp/ls"
  [ "${#files[@]}" -ge 20 ] || die "only ${#files[@]} scripts to check — git ls-files '*.sh' no longer returns what it used to"
  rc=0; git ls-files -- '.github/workflows/*.yml' '.github/actions/*/action.yml' > "$tmp/ls" || rc=$?
  [ "$rc" = 0 ] || die "git ls-files (workflows) failed (exit $rc)"
  while IFS= read -r f; do yml+=("$f"); done < "$tmp/ls"
  [ "${#yml[@]}" -ge 5 ] || die "only ${#yml[@]} workflow / action files to check — the .github layout is not what it used to be"
fi
nscripts=${#files[@]} nyml=${#yml[@]}

# `run:` scripts of workflows and composite actions are cut out and scanned like any other script; map.tsv turns a finding in the
# extracted file back into a line of the YAML (extracted line k is YAML line first+k-2: line 1 is the `set` line the runner implies).
: > "$tmp/map.tsv"
if [ "${#yml[@]}" -gt 0 ]; then
  mkdir "$tmp/runs" || die "mkdir failed"
  rc=0; python3 -I "$here/extract-run-blocks.py" "$tmp/runs" "${yml[@]}" > "$tmp/map.tsv" 2> "$tmp/extract.err" || rc=$?
  [ "$rc" = 0 ] || die "extract-run-blocks.py failed (exit $rc): $(head -2 "$tmp/extract.err")"
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

cat > "$tmp/fnctx.awk" <<'AWK'
# One file at a time. Finds functions defined in the file that run with errexit off (called as `f || x`, `f && x`, `if f`, `while f`,
# `until f`, `! f`, directly or from such a function) and prints a finding for every `x=$(cmd)` in them that has no `||` on the line.
function strip(s,   t) { t = s; gsub(/\047[^\047]*\047/, "", t); gsub(/"[^"]*"/, "", t); sub(/[ \t]#.*/, "", t); return t }
function callpos(t, name,   re, cmd) {   # name sits in command position and an && / || follows it, or a condition keyword precedes it
  cmd = "(^|[;&|{(!]|(then|do|else|elif|if|while|until)[ \t])[ \t]*(![ \t]*)?"
  re = "(^|[;&|{(!]|[ \t])(if|elif|while|until|!)[ \t]+(![ \t]*)?" name "([ \t;)]|$)"
  if (t ~ re) return 1
  re = cmd name "([ \t][^;]*)?[ \t]*(\\|\\||&&)"
  if (t ~ re) return 1
  return 0
}
function anycall(t, name,   re) {
  re = "(^|[;&|{(!]|(then|do|else|elif|if|while|until)[ \t])[ \t]*(![ \t]*)?" name "([ \t;)|&]|$)"
  return t ~ re
}
{
  line = $0
  if (term != "") { t = line; if (dash) sub(/^\t+/, "", t); if (t == term) term = ""; L[NR] = ""; T[NR] = ""; next }   # heredoc bodies are data
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
      e = i
      if (T[i] !~ /\}[ \t]*(;|$|[|&])/ || T[i] ~ /\{[ \t]*$/) { for (e = i + 1; e <= NR; e++) if (index(L[e], ind "}") == 1 && L[e] ~ /^[ \t]*\}/) break }
      nf++; name[nf] = d; from[nf] = i; to[nf] = e; hot[nf] = 0; why[nf] = 0
    }
  }
  for (f = 1; f <= nf; f++)
    for (i = 1; i <= NR; i++) {
      if (i >= from[f] && i <= to[f]) continue
      if (callpos(T[i], name[f])) { hot[f] = 1; why[f] = i; break }
    }
  do {
    ch = 0
    for (f = 1; f <= nf; f++) {
      if (!hot[f]) continue
      for (g = 1; g <= nf; g++) {
        if (hot[g] || g == f) continue
        for (i = from[f] + 1; i <= to[f]; i++) if (anycall(T[i], name[g])) { hot[g] = 1; why[g] = why[f]; ch = 1; break }
      }
    }
  } while (ch)
  for (f = 1; f <= nf; f++) {
    if (!hot[f]) continue
    for (i = from[f]; i <= to[f]; i++) {
      t = T[i]; gsub(/\$\(\(/, "", t)                                      # $(( … )) is arithmetic
      if (t !~ /(^|[;&|{(]|[ \t])[A-Za-z_][A-Za-z_0-9]*(\[[^]]*\])?\+?=("|\047)?(\$\(|`)/) continue
      if (t ~ /(&&|\|\||(^|[;{(]|[ \t])(if|elif|while|until|!))[ \t]*[^;]*[A-Za-z_][A-Za-z_0-9]*\+?=("|\047)?(\$\(|`)/) continue   # the assignment IS the condition: its status is read
      if (t ~ /^[ \t]*(local|declare|typeset|export|readonly)[ \t]/) continue    # declsub's job
      if (L[i] ~ /#[ \t]*exit-ok:[ \t]*[^ \t].../) continue
      code = L[i]; sub(/^[ \t]+/, "", code)
      ok = 0                                                                 # an explicit status check: `|| …` after the substitution's `)`, or `$?` read next
      for (k = i; k <= i + 3 && k <= to[f] && !ok; k++) {
        if (L[k] ~ /\)[ \t]*\|\|/ || (k > i && L[k] ~ /^[ \t]*\|\|/)) ok = 1
        if (k > i && L[k] ~ /\$\?/) ok = 1
        if (k == i && L[k] ~ /\)[ \t]*;[ \t]*[A-Za-z_]*=\$\?/) ok = 1
      }
      if (ok) continue
      printf "%s:%d: fnctx: %s   [%s() runs with errexit off: called at line %d in a ||/&&/if/! position]\n", FILENAME, i, code, name[f], why[f]
      n++
    }
  }
  exit (n > 0)
}
AWK

rc=0; awk -f "$tmp/scan.awk" "${files[@]}" > "$tmp/out" || rc=$?
case $rc in 0 | 1) ;; *) die "awk failed (exit $rc)" ;; esac
for f in "${files[@]}"; do
  rc=0; awk -f "$tmp/fnctx.awk" "$f" >> "$tmp/out" || rc=$?
  case $rc in 0 | 1) ;; *) die "awk (fnctx) failed on $f (exit $rc)" ;; esac
done

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

# findings in an extracted script are reported against the YAML file and line
awk -F'\t' -v map="$tmp/map.tsv" 'FILENAME == map { first[$1] = $3; src[$1] = $2; next }
  { i = index($0, ":"); f = substr($0, 1, i - 1)
    if (f in src) { rest = substr($0, i + 1); j = index(rest, ":"); k = substr(rest, 1, j - 1)
      print src[f] ":" (first[f] + k - 2) ":" substr(rest, j + 1) } else print }' "$tmp/map.tsv" "$tmp/out" > "$tmp/out.mapped" || die "mapping findings failed"
mv "$tmp/out.mapped" "$tmp/out" || die "mv failed"
if [ -s "$tmp/out" ]; then
  cat "$tmp/out" >&2
  echo "check-exit-swallow: $(wc -l < "$tmp/out" | tr -d ' ') finding(s). Fix the line or add '# exit-ok: <why the status cannot change the verdict>' on it (docs/ci.md)." >&2
  exit 1
fi
echo "check-exit-swallow: $nscripts scripts and the run: scripts of $nyml workflow / action files, no swallowed exit status"
