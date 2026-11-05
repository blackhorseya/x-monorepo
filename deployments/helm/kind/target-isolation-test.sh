#!/usr/bin/env bash
# Can anything the kind tasks run reach the CALLER's kubeconfig? (#430, #437, #444, PR #435 r4)
#
#   deployments/helm/kind/target-isolation-test.sh    # needs task, python3 (no PyYAML), perl; `task helm-check` runs it. No cluster, no kind.
#
# r1-r4 each found a shell form the previous gate did not read (unset, read -r, export -n, an absolute path ...). A gate that reads the
# text of a shell script can always be one form short, so this one RUNS the real entry and judges what the tools received:
#
#   - `task --dry` expands the seven kind tasks; every expanded line that names kubectl / helm / kind / a kind script is run with
#     `sh -c` from a scratch copy of deployments/helm/kind, the way the task would run it,
#   - the CALLER is hostile: KUBECONFIG names a decoy file, HOME is a decoy directory with a .kube/config, both chmod 000 (a stand-in
#     for ~/.kube/config pointing at production),
#   - fake kubectl / helm / kind / k6 log which kubeconfig they WOULD use, in the order the real tools do: --kubeconfig, then $KUBECONFIG,
#     then $HOME/.kube/config. No tool is real; nothing here touches a cluster.
#
# Judged: a call that could act on a cluster (kubectl, helm but lint/template/repo/..., kind create|delete|export) used
#   baseline   exactly the task's own file (bin/kind/kubeconfig-<cluster>), none the decoy
#   a mutant   NOT the decoy (a rewrite that does not take effect, or a call that fails, passes; the mutant must really have run)
#
# The mutants edit a COPY of the real smoke.sh before its first kubectl, or the real entry:
#   1 unset KUBECONFIG; kubectl by path         2 read -r KUBECONFIG PATH <<< "<decoy> <second fake bin>:..."   (r4, as the reviewer wrote it)
#   3 export -n PATH KUBECONFIG; bash -c '<kubectl by path>'           4 kubectl by absolute path
#   5 export KUBECONFIG="$HOME/.kube/config"    6 env -u KUBECONFIG kubectl             9 HOME=<decoy home>; unset KUBECONFIG; kubectl by path
#   7 every kind script run directly, outside its task: refused (exit 2), no tool called (#444)
#   8 a Taskfile command that does not go through the runner: check-kubeconfig.py --taskfile must exit non-zero
set -uo pipefail
cd "$(dirname "$0")/../../.."
root=$PWD

command -v perl >/dev/null || { echo "needs perl (alarm)"; exit 1; }
python3 -c 'import yaml' 2>/dev/null || { echo "needs python3 with PyYAML (pip install pyyaml): mutant 8 runs check-kubeconfig.py"; exit 1; }
cluster=probe ns=probe-ns
fail=0 pass=0
ok()  { pass=$((pass + 1)); echo "ok   $*"; }
bad() { fail=$((fail + 1)); echo "FAIL $*"; }

tmp=$(mktemp -d) || exit 1
mutfile="$root/Taskfile.mut-iso-$$.yml"
cleanup() { local rc=$?; case $mutfile in "$root"/Taskfile.mut-iso-*.yml) rm -f "$mutfile" ;; esac
            case $tmp in /*/tmp.*|/tmp/*|/var/folders/*) chmod -R u+rwx "$tmp" 2>/dev/null; rm -rf "$tmp" ;; esac; exit "$rc"; }
trap cleanup EXIT
trap 'exit 130' INT TERM HUP
strip() { sed 's/\x1b\[[0-9;]*m//g'; }

fakebin="$tmp/fakebin" fakebin2="$tmp/fakebin2" calls="$tmp/calls"
decoyhome="$tmp/decoyhome" decoy="$tmp/decoy-kubeconfig" dedicated="$tmp/bin/kind/kubeconfig-$cluster"
tree="$tmp/tree"
mkdir -p "$fakebin" "$fakebin2" "$decoyhome/.kube" "$tmp/bin/kind" "$tree/deployments/helm"
printf 'decoy: caller kubeconfig\n' > "$decoy"; printf 'decoy: ~/.kube/config\n' > "$decoyhome/.kube/config"
chmod 000 "$decoy" "$decoyhome/.kube/config"
printf 'fake kubeconfig\n' > "$dedicated"

# fake cluster tools: log the file they would use; curl jq ... fail, so a script stops at its first step that needs the world
for d in "$fakebin" "$fakebin2"; do
  for t in kubectl helm kind k6; do
    cat > "$d/$t" <<EOF
#!/bin/sh
f="" prev=""
for a in "\$@"; do
  case \$prev in --kubeconfig) f=\$a ;; esac
  case \$a in --kubeconfig=*) f=\${a#--kubeconfig=} ;; esac
  prev=\$a
done
if [ -n "\$f" ]; then src=flag
elif [ -n "\${KUBECONFIG:-}" ]; then f=\$KUBECONFIG; src=env
else f="\${HOME:-/nohome}/.kube/config"; src=home; fi
printf '%s\t%s\t%s\t%s\n' $t "\$f" "\$src" "\$*" >> "$calls"
case "\$1 \$2" in "get kubeconfig") echo "apiVersion: v1" ;; esac
exit 0
EOF
    chmod +x "$d/$t"
  done
done
mkdir -p "$tmp/fake4"; cp "$fakebin/kubectl" "$fakebin/helm" "$fakebin/kind" "$fakebin/k6" "$tmp/fake4/"  # what task's own preconditions need: command -v, nothing else fake
for t in curl jq python3 psql git docker podman openssl; do printf '#!/bin/sh\nexit 1\n' > "$fakebin/$t"; chmod +x "$fakebin/$t"; done
printf '#!/bin/sh\nexit 0\n' > "$fakebin/sleep"; chmod +x "$fakebin/sleep"

fresh_tree() {  # a scratch copy of the kind directory (the mutants edit it, never the repo)
  rm -rf "$tree/deployments/helm/kind"; cp -R "$root/deployments/helm/kind" "$tree/deployments/helm/kind"
  mkdir -p "$tree/scripts"; cp "$root/scripts/best-effort.sh" "$tree/scripts/best-effort.sh"   # the kind scripts source ../../../scripts/best-effort.sh
}

# lines_of <taskfile> → $tmp/lines: the expanded cluster-touching lines of the seven kind tasks
lines_of() {
  : > "$tmp/lines"
  local t
  for t in helm-kind-cluster helm-kind-up helm-kind helm-kind-core helm-kind-payment helm-kind-keda helm-kind-down; do
    # the dry run first, on its own: piped, a task that fails would leave an empty list and every drill below would run nothing
    PATH="$tmp/fake4:$PATH" task -t "$1" --dry "$t" "CLUSTER=$cluster" "NS=$ns" "BIN_DIR=$tmp/bin" > "$tmp/dry.out" 2>&1 || { echo "target-isolation-test: task --dry $t failed" >&2; cat "$tmp/dry.out" >&2; return 1; }
    strip < "$tmp/dry.out" | sed 's/^task: \[[^]]*\] //' \
      | { grep -E '(^|[ ;|&])(kubectl|helm|kind) |deployments/helm/kind/[a-z-]+\.sh' || [ $? -eq 1 ]; } | { grep -v -E 'command -v' || [ $? -eq 1 ]; } >> "$tmp/lines"
  done
  sort -u "$tmp/lines" -o "$tmp/lines"
}

# run_lines [<grep pattern>]: run the (matching) lines from the scratch tree as the hostile caller; the log is $calls
run_lines() {
  : > "$calls"
  local l
  while IFS= read -r l; do
    [ -n "${1:-}" ] && ! printf '%s\n' "$l" | grep -q -- "$1" && continue
    line_rc=0
    ( cd "$tree" && env HOME="$decoyhome" PATH="$fakebin:/usr/bin:/bin" KUBECONFIG="$decoy" \
        perl -e 'alarm 25; exec @ARGV' sh -c "$l" ) >/dev/null 2>&1 || line_rc=$?
    # a task line may fail against the fakes (the verdict is the recorded calls, $calls); the alarm is not that — it means the line never ran to its end
    [ "$line_rc" != 142 ] || echo "target-isolation-test: a task line hit the 25 s alarm: $l" >&2
  done < "$tmp/lines"
}

# judge → "judged=N dedicated=A decoy=D other=O"; exit 0 when nothing used the decoy and nothing but the dedicated file ... (see -s)
judge() {  # judge [strict]: strict = every judged call used the dedicated file
  local out rc
  out=$(python3 - "$calls" "$dedicated" "$decoy" "$decoyhome" "${1:-}" <<'PY' 2>&1
import sys
calls, dedicated, decoy, decoyhome, strict = sys.argv[1:6]
OFFLINE = {"lint", "template", "repo", "version", "dependency", "dep", "package", "show", "pull", "search", "create", "plugin",
           "completion", "env", "verify", "push", "registry", "help"}
n = {"ded": 0, "decoy": 0, "other": 0}
bad = []
for line in open(calls):
    p = line.rstrip("\n").split("\t", 3)
    if len(p) < 4:
        continue
    tool, eff, src, args = p
    a = args.split()
    if tool not in ("kubectl", "helm", "kind"):
        continue
    if tool == "kind" and not (a[:1] and a[0] in ("create", "delete", "export")):
        continue
    if tool == "helm":
        sub = next((w for j, w in enumerate(a) if not w.startswith("-") and (j == 0 or a[j - 1] != "--kubeconfig")), "")
        if sub in OFFLINE:
            continue
    if eff == dedicated:
        n["ded"] += 1
    elif eff == decoy or eff.startswith(decoyhome):
        n["decoy"] += 1
        bad.append("%s used the DECOY (%s via %s) :: %s" % (tool, eff, src, args[:80]))
    else:
        n["other"] += 1
        if strict:
            bad.append("%s used %s via %s, not the task's own file :: %s" % (tool, eff, src, args[:80]))
print("judged=%d dedicated=%d decoy=%d other=%d" % (n["ded"] + n["decoy"] + n["other"], n["ded"], n["decoy"], n["other"]))
for b in bad[:4]:
    print("  " + b)
sys.exit(1 if bad else 0)
PY
  ); rc=$?
  # a judge that broke is not a red verdict: it must print its summary line, and exit 0 or 1
  if [ "$rc" -gt 1 ] || ! printf '%s\n' "$out" | head -1 | grep -q '^judged=[0-9]* dedicated=[0-9]* decoy=[0-9]* other=[0-9]*$'; then
    echo "JUDGE-CRASH"; printf '%s\n' "$out" | tail -3; return 2
  fi
  printf '%s\n' "$out"; return "$rc"
}
field() { head -1 | grep -oE "(^| )$1=[0-9]+" | head -1 | cut -d= -f2; }

# ---- the judge itself (hand-written logs: a decoy use turns it red, a clean log does not, a broken input is a crash, not a verdict)
printf 'kubectl\t%s\tflag\t--kubeconfig x get ns\nk6\tUNSET\tenv\trun\n' "$dedicated" > "$calls"
judge strict >/dev/null 2>&1; j1=$?
printf 'kubectl\t%s\tenv\tget ns\n' "$decoy" > "$calls"
judge >/dev/null 2>&1; j2=$?
printf 'kubectl\t%s/.kube/config\thome\tget ns\n' "$decoyhome" > "$calls"
judge >/dev/null 2>&1; j3=$?
printf 'kubectl\t/tmp/elsewhere\thome\tget ns\n' > "$calls"
judge strict >/dev/null 2>&1; j4=$?
if [ "$j1" = 0 ] && [ "$j2" = 1 ] && [ "$j3" = 1 ] && [ "$j4" = 1 ]; then ok "judge: clean log passes; the decoy file and the decoy HOME turn it red; strict also refuses a third file"
else bad "judge self-check: clean=$j1 (want 0) decoy-file=$j2 decoy-home=$j3 third-file=$j4 (want 1 each)"; fi

printf 'kubectl\t%s\tflag\tget ns\n' "$dedicated" > "$calls"; saved=$calls; calls=/nonexistent/calls
judge >/dev/null 2>&1; j5=$?; calls=$saved
if [ "$j5" = 2 ]; then ok "judge: one that cannot read its log is a crash (exit 2), never a verdict"; else bad "judge crash check: exit $j5, want 2"; fi

# ---- baseline: the real entry, the real scripts, a hostile caller
fresh_tree
lines_of Taskfile.yml
n_lines=$(grep -c . "$tmp/lines")
run_lines
sum=$(judge strict); rc=$?
echo "$sum" | head -1
if [ "$rc" = 0 ]; then ok "baseline: every cluster call of the seven kind tasks and the real scripts used the task's own kubeconfig, none the decoy"
else bad "baseline: $(printf '%s' "$sum" | head -3 | tr '\n' '|')"; fi
judged=$(printf '%s\n' "$sum" | field judged)
if [ "${n_lines:-0}" -ge 20 ] && [ "${judged:-0}" -ge 60 ]; then ok "anchors: $n_lines lines run, $judged cluster calls judged"
else bad "anchors too low: lines=$n_lines judged=${judged:-0} (the run is not seeing the commands it is there for)"; fi

# ---- mutants of the real smoke.sh (run through the real entry: the helm-kind task's smoke line)
mut_smoke() {  # <name> <payload>: insert payload (with a marker that proves it ran) before smoke.sh's first kubectl
  fresh_tree
  rm -f "$tmp/ran-$1"
  python3 - "$tree/deployments/helm/kind/smoke.sh" "$2" "$tmp/ran-$1" <<'PY'
import re, sys
path, payload, marker = sys.argv[1:4]
t = open(path).read()
# the marker first: a payload that the shell refuses (readonly variable) may stop the script under set -e, and "reached" is what the cell needs
new = re.sub(r"(?m)^kubectl ", "echo ran >> '" + marker + "'\n" + payload + "\nkubectl ", t, count=1)
if new == t:
    sys.exit("smoke.sh has no line starting with kubectl")
open(path, "w").write(new)
PY
}
cell() {  # <name> <what> <payload>
  local name=$1 what=$2 payload=$3 out rc
  if ! mut_smoke "$name" "$payload" 2>"$tmp/mut.err"; then bad "mutant $name was not made: $(cat "$tmp/mut.err")"; return; fi
  run_lines 'smoke.sh'
  out=$(judge); rc=$?
  if [ "$rc" = 2 ]; then bad "mutant $name ($what): the judge crashed: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"; return; fi
  if [ ! -s "$tmp/ran-$name" ]; then bad "mutant $name ($what): the edited line was never reached (the cell proves nothing)"; return; fi
  if [ "$rc" = 0 ]; then ok "mutant $name ($what): the edit was reached, the decoy was not used ($(printf '%s' "$out" | head -1))"
  else bad "mutant $name ($what): the decoy WAS used: $(printf '%s' "$out" | sed -n 2p | sed 's/^ *//' | cut -c1-140)"; fi
}
cell 1 'unset KUBECONFIG; kubectl by path (no shim)' "unset KUBECONFIG; $fakebin2/kubectl -n x get ns"
cell 2 'read -r KUBECONFIG PATH (r4)' "read -r KUBECONFIG PATH <<< \"$decoy $fakebin2:/usr/bin:/bin\""
cell 3 'export -n PATH KUBECONFIG; bash -c kubectl by path' "export -n PATH KUBECONFIG; bash -c '$fakebin2/kubectl -n x get ns'"
cell 4 'kubectl by absolute path' "$fakebin2/kubectl -n x get ns"
cell 5 'export KUBECONFIG=$HOME/.kube/config; kubectl by path' "export KUBECONFIG=\"\$HOME/.kube/config\"; $fakebin2/kubectl -n x get ns"
cell 6 'env -u KUBECONFIG kubectl' 'env -u KUBECONFIG kubectl -n x get ns'
cell 9 'HOME=<decoy home>; unset KUBECONFIG; kubectl by path' "HOME=$decoyhome; unset KUBECONFIG; $fakebin2/kubectl -n x get ns"

# ---- controls: the layers are load-bearing. Without the read-only lock, or without the scratch HOME, the same mutants DO reach the decoy
# (a cell that stays green when the layer is gone proves nothing about the layer)
ctrl() {  # <name> <what> <payload> <tweak run in the scratch tree>
  local name=c$1 what=$2 payload=$3 tweak=$4 out rc
  if ! mut_smoke "$name" "$payload" 2>"$tmp/mut.err"; then bad "control $name was not made: $(cat "$tmp/mut.err")"; return; fi
  ( cd "$tree/deployments/helm/kind" && eval "$tweak" ) 2>"$tmp/mut.err" || { bad "control $name: the tweak failed: $(cat "$tmp/mut.err")"; return; }
  run_lines 'smoke.sh'
  out=$(judge); rc=$?
  if [ "$rc" = 2 ]; then bad "control $name ($what): the judge crashed"; return; fi
  if [ ! -s "$tmp/ran-$name" ]; then bad "control $name ($what): the edited line was never reached"; return; fi
  if [ "$rc" = 1 ]; then ok "control $name ($what): the decoy IS used, so the layer is what keeps it out ($(printf '%s' "$out" | sed -n 2p | sed 's/^ *//' | cut -c1-70))"
  else bad "control $name ($what): the decoy was not used even without the layer: the cell does not test it"; fi
}
tweak_nolock() { : > lock.sh; }
tweak_nohome() {  # no lock, and HOME is the caller's (the scripts' own #444 guard has to go too, or it refuses to start)
  : > lock.sh
  sed -i.bak 's|HOME="$scratch/home"|HOME="$real_home"|' run.sh && grep -q 'HOME="$real_home"' run.sh || return 1
  sed -i.bak '/KIND_RUNNER_HOME/d' smoke.sh
}
ctrl 2 'no lock.sh: read -r KUBECONFIG PATH' "read -r KUBECONFIG PATH <<< \"$decoy $fakebin2:/usr/bin:/bin\"" tweak_nolock
ctrl 5 'no lock.sh, caller HOME: export KUBECONFIG=$HOME/.kube/config' "export KUBECONFIG=\"\$HOME/.kube/config\"; $fakebin2/kubectl -n x get ns" tweak_nohome

# ---- 7: every kind script run directly, outside its task, by a hostile caller
fresh_tree
: > "$calls"
direct=0 refused=0 notrefused=""
for f in "$root"/deployments/helm/kind/*.sh; do
  n=$(basename "$f"); case $n in *-test.sh|run.sh|lock.sh) continue ;; esac
  direct=$((direct + 1))
  ( cd "$tree" && env HOME="$decoyhome" PATH="$fakebin:/usr/bin:/bin" KUBECONFIG="$decoy" \
      perl -e 'alarm 25; exec @ARGV' bash "deployments/helm/kind/$n" "$ns" x-monorepo ) >"$tmp/direct.out" 2>&1
  r=$?
  if [ "$r" = 2 ] && grep -q 'task' "$tmp/direct.out"; then refused=$((refused + 1)); else notrefused="$notrefused $n(rc=$r)"; fi
done
sum=$(judge); rc=$?
if [ "$rc" = 2 ]; then bad "mutant 7: the judge crashed"
elif [ "$direct" -ge 8 ] && [ "$refused" = "$direct" ] && [ "$(printf '%s\n' "$sum" | field judged)" = 0 ]; then
  ok "mutant 7: all $direct kind scripts run directly were refused (exit 2, pointing at the task) and called no cluster tool"
else bad "mutant 7: $refused of $direct scripts refused; not refused:${notrefused:- none}; $(printf '%s' "$sum" | head -2 | tr '\n' '|')"; fi

# ---- 8: a Taskfile command that does not go through the runner
mut_taskfile() {
  python3 - "$root/Taskfile.yml" "$mutfile" <<'PY'
import sys
s = open(sys.argv[1]).read()
needle = "  helm-kind-down:\n"
i = s.index(needle)
j = s.index("    cmds:\n", i) + len("    cmds:\n")
s = s[:j] + "      - kubectl --kubeconfig \"{{.KIND_KUBECONFIG}}\" get ns\n" + s[j:]
open(sys.argv[2], "w").write(s)
PY
}
if ! mut_taskfile 2>"$tmp/mut.err"; then bad "mutant 8 was not made: $(cat "$tmp/mut.err")"
else
  python3 "$root/deployments/helm/check-kubeconfig.py" --taskfile "$mutfile" >"$tmp/gate.out" 2>&1; g=$?
  if [ "$g" = 0 ]; then bad "mutant 8: check-kubeconfig.py accepts a kubectl command that does not go through the runner"
  elif ! grep -q '^check-kubeconfig: [0-9]* problem' "$tmp/gate.out"; then bad "mutant 8: the gate crashed instead of giving a verdict: $(tail -2 "$tmp/gate.out" | tr '\n' ' ')"
  else ok "mutant 8: a Taskfile kubectl outside the runner is flagged ($(sed -n 2p "$tmp/gate.out" | sed 's/^ *//' | cut -c1-90))"; fi
fi

echo "kind isolation: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
