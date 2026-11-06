#!/usr/bin/env bash
# Does a write of the kind tasks land in the cluster CLUSTER names, whatever the caller's kubeconfig says? (#437)
#
#   deployments/helm/kind/target-kubeconfig-test.sh     # needs kind, kubectl, task, jq and a container runtime
#                                                       # (CI's helm-kind job runs it before `task helm-kind`)
#
# Two throwaway kind clusters, `target` and `other`, each with a kubeconfig of its own. A third kubeconfig, the ALIAS, is
# `other`'s with its context renamed kind-<target>: the context is called after `target` and points at `other`. That is
# the trap a check of the current-context NAME walks into (PR #435 r0). The probe is the real task entry,
# `task helm-kind-cluster CLUSTER=<target> NS=<ns>`, run with KUBECONFIG=<alias>; what is judged is where the namespace
# it creates (a kubectl write) shows up:
#
#   cell  KUBECONFIG the task is run with          expected
#   C0    -                                        (control) the alias really answers as `other`: kube-system's uid is other's, not target's
#   A1    the alias (context kind-<target> → other)  the namespace is in target and NOT in other
#   P1    target's own kubeconfig (same name, same cluster)   the task goes on: exit 0, the namespace is in target, a second run still passes
#   P2    an empty kubeconfig file                   same as P1: the caller's kubeconfig is not needed at all (never `unset`:
#                                              a regression would then fall back to ~/.kube/config, which can be a production cluster)
#   X1    a DECOY caller kubeconfig whose only cluster is a local listener (the stand-in for a production cluster in ~/.kube/config)
#         the task goes on, the namespace is in target, and the listener was never connected to
#   M1    the alias, on a Taskfile whose namespace command bypasses the runner (a plain kubectl)   the judgement must turn RED: the namespace is in other
#   D1    `task helm-kind-down CLUSTER=<a name that does not exist>` with KUBECONFIG=<a caller file that holds the context kind-<that name>>
#         the caller file is byte-for-byte what it was (kind delete only touches the kubeconfig it is given), and no cluster of this run is gone
#   D3    the same on a Taskfile whose down goes through the runner but has no --kubeconfig flag   the caller file is STILL intact (the runner's
#         environment alone keeps kind off it: the second layer is not what holds)
#   D2    the same on a Taskfile whose down bypasses the runner (a plain kind delete)   the judgement must turn RED: the context is removed from the caller file
#
# Also judged on every cell: the alias file and the caller's ~/.kube/config are byte-for-byte what they were (the task
# never writes the caller's kubeconfig), and the clusters that existed before are all still there.
#
# Nothing here may touch a cluster that is not its own: both names carry a random suffix and must not exist yet, the
# caller's KUBECONFIG is replaced by an empty file for the whole script, every kind / kubectl call names its kubeconfig,
# and the EXIT trap deletes exactly the clusters this run created.
#
# TASKFILE=<file in the repo root> runs the probe cells against another Taskfile (the control and M1 stay as they are).
set -uo pipefail
. "$(dirname "$0")/../../../scripts/best-effort.sh"   # value_of, capture (#462, #466, #470)
type value_of >/dev/null 2>&1 || { echo "target-kubeconfig-test: scripts/best-effort.sh did not load (value_of is missing)" >&2; exit 2; }   # the allowed source line cannot carry `|| exit`, so the load is checked here
# No "$(cmd || echo default)" here: a failed read must not turn into a plausible value (#470).
yesno() { if "$@"; then echo yes; else echo no; fi; }                      # a label for a message: the status IS the answer
fingerprint() { if [ -f "$1" ]; then cksum < "$1"; else echo absent; fi; }    # a file that is not there is "absent"; one that is there and cannot be read fails
num_in() { if [ -f "$1" ]; then cat "$1"; else echo 0; fi; }                  # a counter file the listener has not written yet is 0; an unreadable one fails
lines_in() { if [ -f "$1" ]; then value_of -ok 1 grep -c . "$1"; else echo 0; fi; }   # grep -c: 1 = no line (prints 0); 2+ = the read failed
abs_dir_of() { local p=${1%/*}; [ "$p" != "$1" ] || p=.; ( cd "$p${2:+/$2}" && pwd ); }   # the directory of $0 (plus a subdirectory), absolute: no dirname inside $(…) around a cd
root=$(abs_dir_of "$0" ../../..) || exit 1
cd "$root" || exit 1

TASKFILE=${TASKFILE:-Taskfile.yml}
fail=0 pass=0
ok()  { pass=$((pass + 1)); echo "ok   $*"; }
bad() { fail=$((fail + 1)); echo "FAIL $*"; }

for b in kind kubectl task jq; do
  command -v "$b" >/dev/null || { echo "needs $b"; exit 1; }
done

# every piece of state the trap reads is set BEFORE the trap exists
tmp="" created_target=0 created_other=0 mutfile="" target="" other="" ns="" listener=""
tmp=$(mktemp -d) || exit 1
now=$(date +%s) || exit 1
suffix="$now$$"
target="k437t-$suffix" other="k437o-$suffix" ns="ns437-$suffix"
mutfile="$root/Taskfile.mut437-$suffix.yml"

cleanup() {
  local rc=$?
  [ -n "$listener" ] && kill "$listener" 2>/dev/null
  case $target in k437t-*) [ "$created_target" = 1 ] && kind delete cluster --name "$target" --kubeconfig "$tmp/none.yaml" >/dev/null 2>&1 ;; esac
  case $other in k437o-*) [ "$created_other" = 1 ] && kind delete cluster --name "$other" --kubeconfig "$tmp/none.yaml" >/dev/null 2>&1 ;; esac
  case $mutfile in "$root"/Taskfile.mut437-*.yml) rm -f "$mutfile" ;; esac
  case $target in k437t-*) rm -rf "$root/bin/kind/kubeconfig-$target" "$root/bin/kind/kubeconfig-$target.tmp" "$root/bin/kind/helm-$target" ;; esac
  case $tmp in /*/tmp.*|/tmp/*|/var/folders/*) rm -rf "$tmp" ;; esac
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

# nothing below reads or writes the caller's kubeconfig through the environment
: > "$tmp/none.yaml"
export KUBECONFIG="$tmp/none.yaml"

# Every kubectl in this run goes through a shim that records the kubeconfig it was given (the KUBECONFIG it runs with, or its
# --kubeconfig). Judged at the end: no call ran on the caller's default (~/.kube/config), whose current-context may be production.
real_kubectl=$(type -P kubectl) || { echo "target-kubeconfig-test: no kubectl on PATH" >&2; exit 1; }
mkdir "$tmp/shim"
cat > "$tmp/shim/kubectl" <<SHIM
#!/bin/sh
echo "\${KUBECONFIG-UNSET}|\$*" >> "$tmp/kubectl.calls"
exec "$real_kubectl" "\$@"
SHIM
chmod +x "$tmp/shim/kubectl"
export PATH="$tmp/shim:$PATH"

kind get clusters 2>/dev/null | sort > "$tmp/clusters.before"
if grep -qx -e "$target" -e "$other" "$tmp/clusters.before"; then
  echo "a cluster named $target or $other already exists"; exit 1
fi
user_kc="$HOME/.kube/config"
user_before=$(fingerprint "$user_kc") || exit 1

created_target=1
kind create cluster --name "$target" --kubeconfig "$tmp/target.yaml" --wait 2m >"$tmp/create-target.log" 2>&1 || { cat "$tmp/create-target.log"; echo "cannot create $target"; exit 1; }
created_other=1
kind create cluster --name "$other" --kubeconfig "$tmp/other.yaml" --wait 2m >"$tmp/create-other.log" 2>&1 || { cat "$tmp/create-other.log"; echo "cannot create $other"; exit 1; }

cp "$tmp/other.yaml" "$tmp/alias.yaml"
kubectl --kubeconfig "$tmp/alias.yaml" config rename-context "kind-$other" "kind-$target" >/dev/null || { echo "cannot build the alias kubeconfig"; exit 1; }
alias_before=$(cksum < "$tmp/alias.yaml") || { echo "target-kubeconfig-test: cannot checksum the alias kubeconfig" >&2; exit 1; }

uid() { kubectl --kubeconfig "$1" get ns kube-system -o jsonpath='{.metadata.uid}' 2>/dev/null; }
u_target=$(uid "$tmp/target.yaml") || { echo "cannot read the namespace uid in $target"; exit 1; }
u_other=$(uid "$tmp/other.yaml") || { echo "cannot read the namespace uid in $other"; exit 1; }
u_alias=$(uid "$tmp/alias.yaml") || { echo "cannot read the namespace uid through the alias"; exit 1; }

# C0: the control. An alias that does not answer as `other` would make every cell below prove nothing.
if [ -n "$u_target" ] && [ -n "$u_other" ] && [ "$u_target" != "$u_other" ] && [ "$u_alias" = "$u_other" ]; then
  ok "C0 the alias answers as other (kube-system uid), target is a different cluster"
else
  bad "C0 the alias is not a trap: target=[$u_target] other=[$u_other] alias=[$u_alias]"
  echo "kind target kubeconfig: $pass passed, $fail failed"; exit 1
fi
capture cur cur_rc kubectl --kubeconfig "$tmp/alias.yaml" config current-context || exit 1   # a failed read leaves $cur empty: the C0 line below then fails
[ "$cur" = "kind-$target" ] && ok "C0 the alias's current-context is named kind-$target" || bad "C0 the alias's current-context is [$cur]"

in_cluster() { kubectl --kubeconfig "$1" get ns "$ns" -o name >/dev/null 2>&1; }
reset_ns() {
  kubectl --kubeconfig "$tmp/target.yaml" delete ns "$ns" --ignore-not-found --wait=true >/dev/null 2>&1
  kubectl --kubeconfig "$tmp/other.yaml" delete ns "$ns" --ignore-not-found --wait=true >/dev/null 2>&1
}
# run <taskfile> <KUBECONFIG value>   → exit status of the real task entry, output in $tmp/run.log
run() {
  local tf=$1 kc=$2
  KUBECONFIG="$kc" task -t "$tf" helm-kind-cluster "CLUSTER=$target" "NS=$ns" >"$tmp/run.log" 2>&1
}
# aimed_right: the namespace is in target and not in other
aimed_right() { in_cluster "$tmp/target.yaml" && ! in_cluster "$tmp/other.yaml"; }

# A1
reset_ns
if run "$TASKFILE" "$tmp/alias.yaml"; then rc=0; else rc=$?; fi
if [ "$rc" = 0 ] && aimed_right; then ok "A1 alias (kind-$target → other): the namespace is in target, not in other"
else
  capture a1_t a1_rc yesno in_cluster "$tmp/target.yaml"   # yesno never fails; capture keeps the message free of a $(…) around a command
  capture a1_o a1_rc yesno in_cluster "$tmp/other.yaml"
  bad "A1 rc=$rc, in target: $a1_t, in other: $a1_o"; tail -5 "$tmp/run.log"
fi

# P1: same name, same cluster, twice
reset_ns
if run "$TASKFILE" "$tmp/target.yaml"; then rc1=0; else rc1=$?; fi
if run "$TASKFILE" "$tmp/target.yaml"; then rc2=0; else rc2=$?; fi
if [ "$rc1" = 0 ] && [ "$rc2" = 0 ] && aimed_right; then ok "P1 target's own kubeconfig: the task goes on (twice), the namespace is in target only"
else bad "P1 rc=$rc1/$rc2"; tail -5 "$tmp/run.log"; fi

# P2: an empty kubeconfig
reset_ns
if run "$TASKFILE" "$tmp/none.yaml"; then rc=0; else rc=$?; fi
if [ "$rc" = 0 ] && aimed_right; then ok "P2 an empty kubeconfig: the task goes on, the namespace is in target only"
else bad "P2 rc=$rc"; tail -5 "$tmp/run.log"; fi

# X1: a decoy caller kubeconfig whose server is a local listener that counts connections; the task must not so much as connect
python3 - "$tmp/listener.port" "$tmp/listener.hits" >"$tmp/listener.log" 2>&1 <<'PY' &
import socket, sys
port_file, hits_file = sys.argv[1:3]
s = socket.socket(); s.bind(("127.0.0.1", 0)); s.listen(16)
open(port_file, "w").write(str(s.getsockname()[1]))
n = 0
while True:
    c, _ = s.accept(); n += 1
    open(hits_file, "w").write(str(n)); c.close()
PY
listener=$!
for ((_try158 = 1; _try158 <= 50; _try158++)); do [ -s "$tmp/listener.port" ] && break; sleep 0.1; done
port=$(num_in "$tmp/listener.port") || exit 1
cat > "$tmp/decoy.yaml" <<DECOY
apiVersion: v1
kind: Config
clusters: [{name: prod-asia, cluster: {server: "https://127.0.0.1:$port", insecure-skip-tls-verify: true}}]
users: [{name: u, user: {token: x}}]
contexts: [{name: prod-asia, context: {cluster: prod-asia, user: u}}]
current-context: prod-asia
DECOY
reset_ns
if run "$TASKFILE" "$tmp/decoy.yaml"; then rc=0; else rc=$?; fi
hits=$(num_in "$tmp/listener.hits") || exit 1
if [ "$port" != 0 ] && [ "$rc" = 0 ] && aimed_right && [ "$hits" = 0 ]; then ok "X1 decoy caller kubeconfig (context prod-asia → a listener): the task goes on, the namespace is in target, the listener saw $hits connections"
else bad "X1 rc=$rc port=$port hits=$hits"; tail -5 "$tmp/run.log"; fi
# the listener is alive and counts (a dead one would make the zero above meaningless): connect once
python3 -c "import socket,sys; socket.create_connection(('127.0.0.1', $port), 2).close()" 2>/dev/null; sleep 0.3
hits_now=$(num_in "$tmp/listener.hits") || exit 1
[ "$hits_now" = 1 ] && ok "X1 control: the listener counts a connection" || bad "X1 control: the listener did not count a test connection"
kill "$listener" 2>/dev/null; wait "$listener" 2>/dev/null

# M1: the same probe on a Taskfile that does not point KUBECONFIG anywhere must be red. The judgement is
# aimed_right, the very one A1 passed; the trap has to catch the mutant or the A1 above proves nothing.
sed '/create namespace/s|{{.KIND_RUN}} kubectl --kubeconfig "{{.KIND_KUBECONFIG}}"|kubectl|g' "$root/Taskfile.yml" > "$mutfile"
if cmp -s "$root/Taskfile.yml" "$mutfile"; then
  bad "M1 the mutant Taskfile was not made"
else
  reset_ns
  mut_rc=0; run "$mutfile" "$tmp/alias.yaml" || mut_rc=$?
  echo "M1 mutant run exited $mut_rc (either way is fine: the verdict is aimed_right / in_cluster below)" >&2
  if aimed_right; then bad "M1 the mutant (plain kubectl, no runner) still passes aimed_right: the probe cannot see the alias"
  elif in_cluster "$tmp/other.yaml"; then ok "M1 mutant without the runner: the namespace lands in other → red as it must"
  else
    capture m1_log m1_rc tail -3 "$tmp/run.log"   # a failure message: a failed read leaves it empty, the failure being reported is already known
    bad "M1 the mutant created the namespace nowhere: $m1_log"
  fi
fi
reset_ns

# D1 / D2: kind delete removes the kind-<cluster> context from the kubeconfig it uses ($KUBECONFIG, else ~/.kube/config), whether or not the
# cluster exists. The caller file is a fake made here (other's kubeconfig, its context renamed kind-<ghost>); ghost is no cluster.
ghost="k437x-$suffix"
cp "$tmp/other.yaml" "$tmp/caller.yaml"
kubectl --kubeconfig "$tmp/caller.yaml" config rename-context "kind-$other" "kind-$ghost" >/dev/null || { echo "cannot build the caller kubeconfig"; exit 1; }
caller_before=$(cksum < "$tmp/caller.yaml") || { echo "target-kubeconfig-test: cannot checksum the caller kubeconfig" >&2; exit 1; }
has_ctx() { kubectl --kubeconfig "$tmp/caller.yaml" config get-contexts -o name 2>/dev/null | grep -qx "kind-$ghost"; }
down() { KUBECONFIG="$tmp/caller.yaml" task -t "$1" helm-kind-down "CLUSTER=$ghost" >"$tmp/down.log" 2>&1; }
caller_intact() { local now_sum; has_ctx || return 1; now_sum=$(cksum < "$tmp/caller.yaml") || return 1; [ "$now_sum" = "$caller_before" ]; }
clusters_there() { kind get clusters 2>/dev/null | grep -qx "$target" && kind get clusters 2>/dev/null | grep -qx "$other"; }
if ! has_ctx; then bad "D0 the caller file has no context kind-$ghost (nothing to protect)"; else ok "D0 the caller file holds the context kind-$ghost (no such cluster)"; fi
if down "$TASKFILE"; then rc=0; else rc=$?; fi
if [ "$rc" = 0 ] && caller_intact && clusters_there; then ok "D1 helm-kind-down: the caller kubeconfig is byte-for-byte unchanged, the context is still there, both clusters of this run are too"
else
  capture d1_i d1_rc yesno caller_intact
  capture d1_c d1_rc yesno clusters_there
  bad "D1 rc=$rc intact: $d1_i, clusters: $d1_c"; tail -5 "$tmp/down.log"
fi
mutate_down() {  # <how>: plain = a bare kind delete; noflag = through the runner, without the flag
  python3 - "$root/Taskfile.yml" "$mutfile" "$1" <<'PY'
import sys
src, dst, how = sys.argv[1:4]
s = open(src).read()
old = """'{{.KIND_RUN}} kind delete cluster --name {{.CLUSTER}} --kubeconfig "{{.KIND_KUBECONFIG}}"'"""
new = {"plain": "kind delete cluster --name {{.CLUSTER}}", "noflag": "'{{.KIND_RUN}} kind delete cluster --name {{.CLUSTER}}'"}[how]
if old not in s:
    sys.exit(1)
open(dst, "w").write(s.replace(old, new, 1))
PY
}
if ! mutate_down noflag; then bad "D3 the mutant Taskfile was not made"
else
  mut_rc=0; down "$mutfile" || mut_rc=$?
  echo "mutant down exited $mut_rc (either way is fine: the verdict is caller_intact below)" >&2
  if caller_intact; then ok "D3 down through the runner without the flag: the caller file is still intact (the runner's environment alone holds)"
  else bad "D3 the runner alone did not keep kind off the caller file"; fi
fi
if ! mutate_down plain; then bad "D2 the mutant Taskfile was not made"
else
  mut_rc=0; down "$mutfile" || mut_rc=$?
  echo "mutant down exited $mut_rc (either way is fine: the verdict is caller_intact below)" >&2
  if caller_intact; then bad "D2 the mutant (a plain kind delete) leaves the caller file alone: the probe cannot see it"
  else ok "D2 mutant that bypasses the runner: the context is removed from the caller file → red as it must"; fi
fi

# Judged on the whole run
alias_after=$(cksum < "$tmp/alias.yaml") || exit 1
[ "$alias_after" = "$alias_before" ] && ok "the alias kubeconfig is unchanged" || bad "the alias kubeconfig was written"
user_after=$(fingerprint "$user_kc") || exit 1
[ "$user_after" = "$user_before" ] && ok "the caller's ~/.kube/config is unchanged" || bad "the caller's ~/.kube/config was written"
# no kubectl call ran on the default kubeconfig: each one had a KUBECONFIG inside this run's directory or the task's own
# bin/kind/kubeconfig-<target>, or named its own --kubeconfig; and there were calls to judge (an empty log proves nothing)
calls=$(lines_in "$tmp/kubectl.calls") || exit 1
stray_calls() { grep -v -E "^($tmp/[^|:]*|$root/bin/kind/kubeconfig-$target)\|" "$tmp/kubectl.calls" | grep -v -E -- '--kubeconfig[ =]' | head -3 | tr '\n' ';'; }
capture stray stray_rc stray_calls || exit 1   # pipefail: 1 = grep -v found nothing, which is the good answer; 2+ = a tool broke
[ "$stray_rc" -le 1 ] || { echo "target-kubeconfig-test: reading the kubectl call log failed (exit $stray_rc)" >&2; exit 1; }
if [ "$calls" -ge 10 ] && [ -z "$stray" ]; then ok "all $calls kubectl calls used a kubeconfig of this run (none on ~/.kube/config)"
else bad "kubectl calls: $calls, on another kubeconfig: [$stray]"; fi
kind get clusters 2>/dev/null | sort > "$tmp/clusters.after"
missing=$(comm -23 "$tmp/clusters.before" "$tmp/clusters.after" | tr '\n' ' ') || { echo "target-kubeconfig-test: comparing the cluster lists failed" >&2; exit 1; }
[ -z "$missing" ] && ok "every cluster that existed before is still there" || bad "clusters gone: $missing"

echo "kind target kubeconfig: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
