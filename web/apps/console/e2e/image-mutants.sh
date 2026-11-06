#!/usr/bin/env bash
# Fault injection for the image project (#449): the specs that stay on the image must go red when nginx is
# broken. Each mutant is a copy of the image's build context (Dockerfile, deploy/, dist/) with ONE setting broken;
# e2e/image.sh builds it (E2E_IMAGE_CONTEXT) and runs the image project against it. The control (no mutation)
# has to be green, or "every mutant is red" could just mean the harness is always red.
#
#   task web-e2e-image-mutants                 # all of them
#   task web-e2e-image-mutants -- fallback     # only the named ones
#
# Output: one line per mutant, `mutant <name>: RED (<failed tests>)` / `GREEN`; exit 1 if the control is red
# or a mutant is green. Which spec files failed is printed so that "A stays green, B goes red" can be read off.
set -euo pipefail
abs_dir_of() { local p=${1%/*}; [ "$p" != "$1" ] || p=.; ( cd "$p${2:+/$2}" && pwd ); }   # the absolute directory of a path, without dirname inside $(…)
app_dir=$(abs_dir_of "$0" ..) || exit 1
cd "$app_dir" || exit 1
[ -f dist/index.html ] || { echo "image-mutants: dist/ missing, run task web-build" >&2; exit 1; }

work=$(mktemp -d "${TMPDIR:-/tmp}/console-mutants.XXXXXX")
trap 'rm -rf "$work"' EXIT

# mutate <name> <file under deploy/> <sed expression>: prepares $work/<name> and fails if the sed changed nothing
# (a mutant that does not differ from the original would be a false "GREEN").
mutate() {
  local name=$1 file=$2 expr=$3 d=$work/$1
  mkdir -p "$d"
  cp Dockerfile "$d/"; cp -R deploy "$d/deploy"; cp -R dist "$d/dist"
  sed -E -i.bak "$expr" "$d/deploy/$file"
  if cmp -s "$d/deploy/$file" "$d/deploy/$file.bak"; then echo "image-mutants: mutant $name changed nothing in deploy/$file" >&2; exit 2; fi
  rm -f "$d/deploy/$file.bak"
}

mutate fallback default.conf.template 's|try_files \$uri /index.html;|try_files $uri =404;|'
mutate api-proxy default.conf.template 's|proxy_pass \$\{RPC_UPSTREAM\};|return 502;|'
mutate csp-script-none security-headers.inc.template "s|script-src 'self'|script-src 'none'|"
mutate csp-sandbox security-headers.inc.template "s|frame-ancestors 'none'\"|frame-ancestors 'none'; sandbox\"|"
mutate config-json config.json.template 's|"apiKeyLogin":|"apiKeyLoginX":|'
mutate asset-mime default.conf.template 's|(location /assets/ \{)|\1 default_type text/plain; types { }|'
mutate asset-path default.conf.template 's|location /assets/ \{|location /assetz/ {|'

want=("$@")
rc=0
# run <name> <context>: 0 = the image project went red on a SPEC, 1 = green, 2 = the harness broke (e2e/image-mutants.lib.sh:
# no Playwright summary, a parsing stage failed, the evidence copy failed). Never "red" because something else went wrong.
# shellcheck source=image-mutants.lib.sh
. ./e2e/image-mutants.lib.sh
image_sh=${E2E_IMAGE_SH:-./e2e/image.sh}
run() {
  local name=$1 ctx=$2 log=$work/$1.log code=0 st=0 keep=${E2E_MUTANT_LOGS:-$work}
  # --retries=0: a mutant has to be red on the first attempt, and a failing test is not run twice (30s timeouts add up)
  E2E_IMAGE_CONTEXT=$ctx CI=1 "$image_sh" --retries=0 >"$log" 2>&1 || code=$?
  keep_log "$log" "$keep" "$name" || { echo "mutant $name: ERROR, cannot keep the log in $keep"; return 2; }
  classify "$name" "$log" "$code" || st=$?
  if [ "$st" -eq 2 ]; then tail -5 "$log"; fi
  return "$st"
}
selected() { [ ${#want[@]} -eq 0 ] && return 0; local w; for w in "${want[@]}"; do [ "$w" = "$1" ] && return 0; done; return 1; }

if selected control; then
  c=0; run control . || c=$?
  # the control has to be green: 1 from run. 0 (red) or 2 (broken) means the harness proves nothing.
  if [ "$c" -eq 1 ]; then echo "control: GREEN (as required)"; else echo "control: not green (run=$c), the harness cannot prove anything"; exit 1; fi
fi
for name in fallback api-proxy csp-script-none csp-sandbox config-json asset-mime asset-path; do
  selected "$name" || continue
  c=0; run "$name" "$work/$name" || c=$?
  case $c in
    0) echo "mutant $name: RED (as required)" ;;
    1) echo "mutant $name: GREEN  <-- the image specs did not notice"; rc=1 ;;
    *) echo "mutant $name: ERROR (harness)"; rc=1 ;;
  esac
done
exit $rc
