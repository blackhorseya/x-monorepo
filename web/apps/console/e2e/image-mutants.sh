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
cd "$(dirname "$0")/.."
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
run() { # run <name> <context>
  local name=$1 ctx=$2 log=$work/$1.log code=0
  # --retries=0: a mutant has to be red on the first attempt, and a failing test is not run twice (30s timeouts add up)
  E2E_IMAGE_CONTEXT=$ctx CI=1 ./e2e/image.sh --retries=0 >"$log" 2>&1 || code=$?
  local failed
  failed=$(sed -E 's/\x1b\[[0-9;]*m//g' "$log" | grep -E '^\s+✘\s+[0-9]+ \[image\]' | sed -E 's/^.*\[image\] › e2e\/([^:]+):.*/\1/' | sort | uniq -c | awk '{printf "%s%s×%s", s, $2, $1; s=", "}')
  echo "mutant $name: exit=$code failed-spec-files=[${failed}]"
  cp "$log" "${E2E_MUTANT_LOGS:-$work}/$name.log" 2>/dev/null || true
  return $code
}
selected() { [ ${#want[@]} -eq 0 ] && return 0; local w; for w in "${want[@]}"; do [ "$w" = "$1" ] && return 0; done; return 1; }

if selected control; then
  run control . && echo "control: GREEN (as required)" || { echo "control: RED, the harness cannot prove anything"; exit 1; }
fi
for name in fallback api-proxy csp-script-none csp-sandbox config-json asset-mime asset-path; do
  selected "$name" || continue
  if run "$name" "$work/$name"; then echo "mutant $name: GREEN  <-- the image specs did not notice"; rc=1; else echo "mutant $name: RED (as required)"; fi
done
exit $rc
