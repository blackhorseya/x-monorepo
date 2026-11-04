#!/usr/bin/env bash
# What kubeconfig does every cluster call of the kind tasks REALLY get, when the commands are run? (#437, PR #435 r2)
#
#   deployments/helm/kind/target-exec-test.sh     # needs task, python3 + PyYAML; `task helm-check` runs it. No cluster, no kind.
#
# check-kubeconfig.py reads the commands; this runs them. `task --dry` expands each kind task, and every expanded line that
# names kubectl / helm / kind / a kind script is run with `sh -c`, from a scratch directory, with
#   - fake kubectl / helm / kind first on PATH that log the KUBECONFIG they run with and their arguments,
#   - fake deployments/helm/kind/*.sh that do what a hostile script does (`unset KUBECONFIG`, then kubectl from a subshell, from
#     xargs and from `bash -c`), so what reaches the fake kubectl is what the real shim makes of it,
#   - KUBECONFIG set to a decoy file the way a caller's would be (and HOME an empty directory, nothing reads ~/.kube/config).
# Judged: every kubectl / helm call, and every `kind create|delete|export`, got exactly one `--kubeconfig` and it is the task's
# own file (bin/kind/kubeconfig-<cluster>); none got the decoy, none ran with the variable unset and no flag.
#
# The REAL kind scripts are run too (smoke.sh payment.sh payment-flow.sh negative.sh outage.sh scale.sh coredns-dex.sh
# payment-upgrade-guard.sh), behind the same prefix, with fake kubectl / helm / kind / curl / jq / ...: each runs until its first
# failed step (a few seconds), and every kubectl / helm call it made before that must have arrived with the task's flag. That is
# the part of each script that runs without a cluster; the rest is the static scan's. Two mutants edit a COPY of a real script
# (an absolute-path kubectl in smoke.sh, an absolute-path helm in payment-flow.sh): the copy is run, and scanned.
#
# Then the same judgement on mutants of the real Taskfile, each made by one edit: the commands are run, and what the fake
# kubectl receives must be WRONG (the mutant is dangerous), and check-kubeconfig.py --taskfile <mutant> must exit non-zero
# (the gate sees it). A gate that lets a dangerous mutant through shows up here as a mutant that is "dangerous and not flagged".
#
#   a  KUBE_ENV; unset KUBECONFIG; kubectl …      b  KUBE_ENV; export KUBECONFIG=<decoy>; kubectl …
#   c  kubectl …; KUBE_ENV  (the guard after the operation)    d  the flag on another command of the same line
#   e  --kubeconfig=""      f  a kind script without KUBE_ENV    g  KUBE_ENV without the shim directory on PATH
#   h  kind delete without --kubeconfig                          i  --kubeconfig naming the decoy
#   j  smoke.sh: kubectl by absolute path    k  payment-flow.sh: helm by absolute path      (run AND scanned)
#   l  scale.sh: command -p kubectl   m  negative.sh: $(command -v kubectl)   n  outage.sh: sources a file   (scanned)
#
# The judge asks for the flag on every call, the strictest reading. A call by absolute path in a script that still has
# KUBECONFIG exported would reach the right file anyway; it is a violation because it is the first half of a bypass (the second
# half, unset / re-export, is also scanned), and a gate that tolerated it would have to be re-argued at every later edit.
#
# TASKFILE=<file in the repo root> runs the baseline against another Taskfile (the mutants are always made from Taskfile.yml).
set -uo pipefail
cd "$(dirname "$0")/../../.."
root=$PWD

TASKFILE=${TASKFILE:-Taskfile.yml}
python3 -c 'import yaml' 2>/dev/null || { echo "needs python3 with PyYAML (pip install pyyaml): check-kubeconfig.py cannot run without it"; exit 1; }
cluster=probe ns=probe-ns
fail=0 pass=0
ok()  { pass=$((pass + 1)); echo "ok   $*"; }
bad() { fail=$((fail + 1)); echo "FAIL $*"; }

tmp="" mutfile=""
tmp=$(mktemp -d) || exit 1
mutfile="$root/Taskfile.mut-exec-$$.yml"
cleanup() { local rc=$?; case $mutfile in "$root"/Taskfile.mut-exec-*.yml) rm -f "$mutfile" ;; esac
            case $tmp in /*/tmp.*|/tmp/*|/var/folders/*) rm -rf "$tmp" ;; esac; exit "$rc"; }
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

mkdir -p "$tmp/fakebin" "$tmp/work/deployments/helm/kind" "$tmp/bin/kind" "$tmp/home"
decoy="$tmp/decoy.yaml"; printf 'decoy\n' > "$decoy"
expected="$tmp/bin/kind/kubeconfig-$cluster"
calls="$tmp/calls"

# fake tools: log, succeed. `kind get clusters` finds nothing (so kind create runs); `kind get kubeconfig` prints a file.
for t in kubectl helm kind k6; do
  cat > "$tmp/fakebin/$t" <<EOF
#!/bin/sh
printf '%s\t%s\t%s\n' "$t" "\${KUBECONFIG-UNSET}" "\$*" >> "$calls"
case "\$1 \$2" in "get kubeconfig") echo "fake kubeconfig" ;; esac
exit 0
EOF
  chmod +x "$tmp/fakebin/$t"
done
# fake kind scripts: log, then behave like a script that tries to lose the kubeconfig
for f in "$root"/deployments/helm/kind/*.sh; do
  n=$(basename "$f"); case $n in *-test.sh) continue ;; esac
  cat > "$tmp/work/deployments/helm/kind/$n" <<EOF
#!/usr/bin/env bash
printf 'script\t%s\t%s\n' "\${KUBECONFIG-UNSET}" "$n" >> "$calls"
unset KUBECONFIG
kubectl get ns script-direct
( kubectl get ns script-subshell )
bash -c 'kubectl get ns script-bash-c'
echo script-xargs | xargs kubectl get ns
helm -n x status r
exit 0
EOF
  chmod +x "$tmp/work/deployments/helm/kind/$n"
done
printf 'fake kubeconfig\n' > "$expected"

strip() { sed 's/\x1b\[[0-9;]*m//g'; }

# lines_of <taskfile> → the expanded cluster-touching lines of the five kind tasks, one per line, in $tmp/lines
lines_of() {
  : > "$tmp/lines"
  local t
  for t in helm-kind-cluster helm-kind-up helm-kind helm-kind-keda helm-kind-down; do
    PATH="$tmp/fakebin:$PATH" task -t "$1" --dry "$t" "CLUSTER=$cluster" "NS=$ns" "BIN_DIR=$tmp/bin" 2>&1 | strip \
      | sed 's/^task: \[[^]]*\] //' \
      | grep -E '(^|[ ;|&])(kubectl|helm|kind) |deployments/helm/kind/[a-z-]+\.sh' | grep -v -E 'command -v|^kind (get kubeconfig|load)' >> "$tmp/lines" || true
  done
  sort -u "$tmp/lines" -o "$tmp/lines"
}

# run_lines → runs every line in $tmp/lines under the fakes; calls in $tmp/calls
run_lines() {
  : > "$calls"
  local l
  while IFS= read -r l; do
    ( cd "$tmp/work" && env -i HOME="$tmp/home" PATH="$tmp/fakebin:/usr/bin:/bin" KUBECONFIG="$decoy" sh -c "$l" ) >/dev/null 2>&1 || true
  done < "$tmp/lines"
}

# judge → prints a summary line; exit 0 when every call got exactly the task's own kubeconfig
judge() {
  local out rc
  out=$(judge_py 2>&1); rc=$?
  if [ "$rc" -gt 1 ] || ! printf '%s\n' "$out" | head -1 | grep -q '^kubectl=[0-9]* helm=[0-9]* kind=[0-9]* scripts=[0-9]* wrong=[0-9]*$'; then
    echo "JUDGE-CRASH"; printf '%s\n' "$out" | tail -3; return 2
  fi
  printf '%s\n' "$out"; return "$rc"
}
judge_py() {
  python3 - "$calls" "$expected" "$decoy" <<'PY'
import sys
calls, expected, decoy = sys.argv[1:4]
n = {"kubectl": 0, "helm": 0, "kind": 0, "script": 0}
wrong = []
for line in open(calls):
    parts = line.rstrip("\n").split("\t", 2)
    if len(parts) < 3:
        continue
    tool, env, args = parts
    a = args.split()
    if tool == "script":
        n["script"] += 1
        continue
    if tool not in ("kubectl", "helm", "kind"):
        continue  # k6 and the like: logged by a fake, not judged
    if tool == "kind" and not (a[:1] and a[0] in ("create", "delete", "export")):
        continue
    if tool == "helm":  # the subcommands that never reach a cluster (check-kubeconfig.py's HELM_OFFLINE)
        sub = next((w for j, w in enumerate(a) if not w.startswith("-") and (j == 0 or a[j - 1] != "--kubeconfig")), "")
        if sub in ("lint", "template", "repo", "version", "dependency", "dep", "package", "show", "pull", "search", "create", "plugin", "completion", "env", "verify", "push", "registry", "help"):
            continue
    n[tool] += 1
    flags = [a[i + 1] if i + 1 < len(a) else "" for i, w in enumerate(a) if w == "--kubeconfig"] + \
            [w.split("=", 1)[1] for w in a if w.startswith("--kubeconfig=")]
    if flags != [expected]:
        wrong.append("%s env=%s flags=%r :: %s" % (tool, "decoy" if env == decoy else env, flags, args[:90]))
print("kubectl=%d helm=%d kind=%d scripts=%d wrong=%d" % (n["kubectl"], n["helm"], n["kind"], n["script"], len(wrong)))
for w in wrong[:4]:
    print("  " + w)
sys.exit(1 if wrong else 0)
PY
}
count_of() { judge | sed -n '1s/.*\b'"$2"'=\([0-9]*\).*/\1/p'; }

# fakes for running the REAL scripts: the cluster tools log and succeed; the others fail, so a script stops at its first step that needs
# the world (curl, jq, psql, git, docker ...) and sleep returns at once
mkdir -p "$tmp/scriptbin"
for t in kubectl helm kind k6; do cp "$tmp/fakebin/$t" "$tmp/scriptbin/$t"; done
for t in curl jq python3 psql git docker podman openssl; do printf '#!/bin/sh\nexit 1\n' > "$tmp/scriptbin/$t"; chmod +x "$tmp/scriptbin/$t"; done
printf '#!/bin/sh\nexit 0\n' > "$tmp/scriptbin/sleep"; chmod +x "$tmp/scriptbin/sleep"

# run_real <kind dir>: every real script under the environment KUBE_ENV makes (KUBECONFIG, KIND_KUBECONFIG, the shims first on PATH)
run_real() {
  : > "$calls"
  local sc
  for sc in smoke payment payment-flow negative outage scale coredns-dex payment-upgrade-guard; do
    ( cd "$root" && env -i HOME="$tmp/home" PATH="$root/deployments/helm/kind/shim:$tmp/scriptbin:/usr/bin:/bin" \
        KIND_KUBECONFIG="$expected" KUBECONFIG="$expected" \
        perl -e 'alarm 20; exec @ARGV' bash "$1/$sc.sh" "$ns" x-monorepo deployments/helm/x-monorepo deployments/helm/kind/values.yaml deployments/helm/kind/payment-flow-values.yaml ) >/dev/null 2>&1 || true
  done
}

# ---- the judge itself, on call logs written by hand (a tool it does not judge must not crash it; a wrong call must turn it red)
printf 'kubectl\t%s\t--kubeconfig %s get ns\nk6\tUNSET\trun x.js\nscript\tUNSET\tsmoke.sh\n' "$expected" "$expected" > "$calls"
judge >/dev/null 2>&1; jr=$?
printf 'kubectl\t%s\t--kubeconfig %s get ns\nk6\tUNSET\trun x.js\nhelm\t%s\t-n x status r\n' "$expected" "$expected" "$decoy" > "$calls"
judge >/dev/null 2>&1; jw=$?
if [ "$jr" = 0 ] && [ "$jw" = 1 ]; then ok "judge: a clean log passes (k6 and script lines do not crash it), a call without the flag turns it red"
else bad "judge self-check: clean log rc=$jr (want 0), wrong log rc=$jw (want 1)"; fi

# ---- baseline: the real Taskfile
lines_of "$TASKFILE"
n_lines=$(grep -c . "$tmp/lines")
run_lines
sum=$(judge); rc=$?
echo "$sum" | head -1
if [ "$rc" = 0 ]; then ok "baseline: every kubectl / helm / kind create|delete call got the task's own kubeconfig, none the decoy"
else bad "baseline: $sum"; fi
# anchors: the run saw the calls it is there for (an empty log judges nothing)
k=$(judge | sed -n '1s/.*kubectl=\([0-9]*\).*/\1/p'); h=$(judge | sed -n '1s/.*helm=\([0-9]*\).*/\1/p')
ki=$(judge | sed -n '1s/.*kind=\([0-9]*\).*/\1/p'); sc=$(judge | sed -n '1s/.*scripts=\([0-9]*\).*/\1/p')
if [ "${n_lines:-0}" -ge 20 ] && [ "${k:-0}" -ge 30 ] && [ "${h:-0}" -ge 8 ] && [ "${ki:-0}" -ge 2 ] && [ "${sc:-0}" -ge 8 ]; then
  ok "anchors: $n_lines lines run, kubectl=$k helm=$h kind=$ki scripts=$sc (including their hostile unset / subshell / bash -c / xargs calls)"
else bad "anchors too low: lines=$n_lines kubectl=$k helm=$h kind=$ki scripts=$sc"; fi

# ---- the real scripts, run
run_real "$root/deployments/helm/kind"
sumr=$(judge); rcr=$?
echo "$sumr" | head -1
kr=$(judge | sed -n '1s/.*kubectl=\([0-9]*\).*/\1/p'); hr=$(judge | sed -n '1s/.*helm=\([0-9]*\).*/\1/p')
if [ "$rcr" = 0 ]; then ok "real scripts: every kubectl / helm call they made before their first failed step arrived with the task's own --kubeconfig"
else bad "real scripts: $sumr"; fi
if [ "${kr:-0}" -ge 12 ] && [ "${hr:-0}" -ge 2 ]; then ok "anchors (real scripts): kubectl=$kr helm=$hr calls came out of the real scripts"
else bad "anchors (real scripts) too low: kubectl=$kr helm=$hr"; fi

# ---- mutants: made from the real Taskfile.yml by one edit each
mutate() {  # mutate <name> <python expression on s (the file text) returning the new text>
  python3 - "$root/Taskfile.yml" "$mutfile" "$decoy" "$1" <<'PY'
import sys
src, dst, decoy, name = sys.argv[1:5]
s = open(src).read()
F = '--kubeconfig "{{.KIND_KUBECONFIG}}"'
DEPS = "kubectl %s -n {{.NS}} apply -f {{.KIND_DIR}}/deps.yaml" % F
SMOKE = "'{{.KUBE_ENV}}; {{.KIND_DIR}}/smoke.sh {{.NS}} x-monorepo'"
def swap(old, new, count=1):
    global s
    if old not in s:
        sys.exit("mutant %s: the text to edit is gone: %r" % (name, old[:60]))
    s = s.replace(old, new, count)
GUARD = 'test -s "{{.KIND_KUBECONFIG}}" || exit 1; export KUBECONFIG="{{.KIND_KUBECONFIG}}"'  # what KUBE_ENV was in r2
if name == "a":
    swap(DEPS, GUARD + "; unset KUBECONFIG; kubectl -n {{.NS}} apply -f {{.KIND_DIR}}/deps.yaml")
elif name == "b":
    swap(DEPS, GUARD + "; export KUBECONFIG=" + decoy + "; kubectl -n {{.NS}} apply -f {{.KIND_DIR}}/deps.yaml")
elif name == "c":
    swap(DEPS, "kubectl -n {{.NS}} apply -f {{.KIND_DIR}}/deps.yaml; " + GUARD)
elif name == "d":
    swap(DEPS, "kubectl %s get ns >/dev/null; kubectl -n {{.NS}} apply -f {{.KIND_DIR}}/deps.yaml" % F)
elif name == "e":
    swap(DEPS, 'kubectl --kubeconfig="" -n {{.NS}} apply -f {{.KIND_DIR}}/deps.yaml')
elif name == "f":
    swap(SMOKE, "'{{.KIND_DIR}}/smoke.sh {{.NS}} x-monorepo'")
elif name == "g":
    import re
    s = re.sub(r' PATH="[^"]*/deployments/helm/kind/shim:\$PATH"', "", s, count=1)
    if "kind/shim" in s.split("KUBE_ENV:")[1].split("\n")[0]:
        sys.exit("mutant g: the shim is still in KUBE_ENV")
elif name == "h":
    swap('kind delete cluster --name {{.CLUSTER}} %s' % F, 'kind delete cluster --name {{.CLUSTER}}')
elif name == "i":
    swap(DEPS, "kubectl --kubeconfig " + decoy + " -n {{.NS}} apply -f {{.KIND_DIR}}/deps.yaml")
else:
    sys.exit("no mutant " + name)
open(dst, "w").write(s)
PY
}

for m in a b c d e f g h i; do
  if ! mutate "$m" 2>"$tmp/mut.err"; then bad "mutant $m was not made: $(cat "$tmp/mut.err")"; continue; fi
  python3 "$root/deployments/helm/check-kubeconfig.py" --taskfile "$mutfile" >"$tmp/gate.out" 2>&1; gate_rc=$?
  if [ "$gate_rc" != 0 ] && ! grep -q '^check-kubeconfig: [0-9]* problem' "$tmp/gate.out"; then
    bad "mutant $m: the gate crashed instead of giving a verdict: $(tail -2 "$tmp/gate.out" | tr '\n' ' ')"; continue
  fi
  lines_of "$mutfile"; run_lines
  judge >"$tmp/judge.out" 2>&1; judge_rc=$?
  if [ "$judge_rc" = 2 ]; then bad "mutant $m: the judge crashed: $(tr '\n' ' ' < "$tmp/judge.out" | cut -c1-200)"; continue; fi
  case $m in
    a|b|c|d|e) want='apply -f deployments/helm/kind/deps.yaml' ;;
    i) want='decoy.yaml' ;;
    f|g) want='script-' ;;
    h) want='delete cluster --name' ;;
  esac
  if [ "$judge_rc" != 0 ] && ! sed -n 2p "$tmp/judge.out" | grep -q -F -e "$want"; then
    bad "mutant $m: the first wrong call is not the edited one (wanted [$want]): $(sed -n 2p "$tmp/judge.out")"
  elif [ "$gate_rc" != 0 ] && [ "$judge_rc" != 0 ]; then
    ok "mutant $m: run → $(sed -n 2p "$tmp/judge.out" | sed 's/^ *//' | cut -c1-120) | gate → $(sed -n 2p "$tmp/gate.out" | sed 's/^ *//' | cut -c1-80)"
  elif [ "$gate_rc" = 0 ] && [ "$judge_rc" != 0 ]; then bad "mutant $m is dangerous when run and the gate lets it through: $(sed -n 2p "$tmp/judge.out")"
  elif [ "$gate_rc" != 0 ] && [ "$judge_rc" = 0 ]; then bad "mutant $m: the gate is red but running it shows nothing wrong (the mutant is not the danger it was made to be)"
  else bad "mutant $m: neither the gate nor the run sees it"; fi
done

# ---- mutants of the real scripts: a copy of deployments/helm/kind with one edit, scanned and (j k) run
mk_script_mutant() {  # <name> → $tmp/mutkind
  rm -rf "$tmp/mutkind"; cp -R "$root/deployments/helm/kind" "$tmp/mutkind"
  local f
  case $1 in
    j) f=smoke.sh ;;
    k) f=payment-flow.sh ;;
    l) f=scale.sh ;;
    m) f=negative.sh ;;
    n) f=outage.sh ;;
  esac
  python3 - "$tmp/mutkind/$f" "$1" "$tmp" <<'PY'
import re, sys
path, name, tmp = sys.argv[1:4]
t = open(path).read()
before = t
if name == "j":
    t = re.sub(r"(?m)^kubectl ", tmp + "/scriptbin/kubectl ", t, count=1)
elif name == "k":
    t = re.sub(r"(?m)^(\s*)helm ", r"\1" + tmp + "/scriptbin/helm ", t, count=1)
elif name == "l":
    t = re.sub(r"(?m)^kubectl ", "command -p kubectl ", t, count=1)
elif name == "m":
    t = re.sub(r"(?m)^kubectl ", 'k=$(command -v kubectl); "$k" ', t, count=1)
elif name == "n":
    t = t.replace("set -", "source ./lib.sh\nset -", 1)
if t == before:
    sys.exit("mutant %s: nothing to edit in %s" % (name, path))
open(path, "w").write(t)
PY
}
for m in j k l m n; do
  if ! mk_script_mutant "$m" 2>"$tmp/mut.err"; then bad "script mutant $m was not made: $(cat "$tmp/mut.err")"; continue; fi
  python3 "$root/deployments/helm/check-kubeconfig.py" --kind-dir "$tmp/mutkind" >"$tmp/gate.out" 2>&1; gate_rc=$?
  if [ "$gate_rc" != 0 ] && ! grep -q '^check-kubeconfig: [0-9]* problem' "$tmp/gate.out"; then
    bad "script mutant $m: the gate crashed instead of giving a verdict: $(tail -2 "$tmp/gate.out" | tr '\n' ' ')"; continue
  fi
  verdict=$(sed -n 2p "$tmp/gate.out" | sed 's/^ *//' | cut -c1-110)
  case $m in
    j|k)
      run_real "$tmp/mutkind"; judge >"$tmp/judge.out" 2>&1; judge_rc=$?
      if [ "$judge_rc" = 2 ]; then bad "script mutant $m: the judge crashed: $(tr '\n' ' ' < "$tmp/judge.out" | cut -c1-200)"; continue; fi
      if [ "$gate_rc" != 0 ] && [ "$judge_rc" != 0 ]; then ok "script mutant $m: run → $(sed -n 2p "$tmp/judge.out" | sed 's/^ *//' | cut -c1-70) | gate → $verdict"
      elif [ "$gate_rc" = 0 ] && [ "$judge_rc" != 0 ]; then bad "script mutant $m is dangerous when run and the gate lets it through"
      else bad "script mutant $m: gate rc=$gate_rc, run rc=$judge_rc (the edit did not reach a call, or the gate is blind)"; fi ;;
    *)
      if [ "$gate_rc" != 0 ]; then ok "script mutant $m: gate → $verdict"
      else bad "script mutant $m: the gate lets it through"; fi ;;
  esac
done

echo "kind exec: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
