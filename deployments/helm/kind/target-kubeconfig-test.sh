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
#   M1    the alias, on a Taskfile with the KUBECONFIG prefix removed (KUBE_ENV: ':')   the judgement must turn RED: the namespace is in other
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
cd "$(dirname "$0")/../../.."
root=$PWD

TASKFILE=${TASKFILE:-Taskfile.yml}
fail=0 pass=0
ok()  { pass=$((pass + 1)); echo "ok   $*"; }
bad() { fail=$((fail + 1)); echo "FAIL $*"; }

for b in kind kubectl task jq; do
  command -v "$b" >/dev/null || { echo "needs $b"; exit 1; }
done

# every piece of state the trap reads is set BEFORE the trap exists
tmp="" created_target=0 created_other=0 mutfile="" target="" other="" ns=""
tmp=$(mktemp -d) || exit 1
suffix="$(date +%s)$$"
target="k437t-$suffix" other="k437o-$suffix" ns="ns437-$suffix"
mutfile="$root/Taskfile.mut437-$suffix.yml"

cleanup() {
  local rc=$?
  case $target in k437t-*) [ "$created_target" = 1 ] && kind delete cluster --name "$target" --kubeconfig "$tmp/none.yaml" >/dev/null 2>&1 ;; esac
  case $other in k437o-*) [ "$created_other" = 1 ] && kind delete cluster --name "$other" --kubeconfig "$tmp/none.yaml" >/dev/null 2>&1 ;; esac
  case $mutfile in "$root"/Taskfile.mut437-*.yml) rm -f "$mutfile" ;; esac
  case $target in k437t-*) rm -f "$root/bin/kind/kubeconfig-$target" "$root/bin/kind/kubeconfig-$target.tmp" ;; esac
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
real_kubectl=$(command -v kubectl)
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
user_before=$(cksum < "$user_kc" 2>/dev/null || echo absent)

created_target=1
kind create cluster --name "$target" --kubeconfig "$tmp/target.yaml" --wait 2m >"$tmp/create-target.log" 2>&1 || { cat "$tmp/create-target.log"; echo "cannot create $target"; exit 1; }
created_other=1
kind create cluster --name "$other" --kubeconfig "$tmp/other.yaml" --wait 2m >"$tmp/create-other.log" 2>&1 || { cat "$tmp/create-other.log"; echo "cannot create $other"; exit 1; }

cp "$tmp/other.yaml" "$tmp/alias.yaml"
kubectl --kubeconfig "$tmp/alias.yaml" config rename-context "kind-$other" "kind-$target" >/dev/null || { echo "cannot build the alias kubeconfig"; exit 1; }
alias_before=$(cksum < "$tmp/alias.yaml")

uid() { kubectl --kubeconfig "$1" get ns kube-system -o jsonpath='{.metadata.uid}' 2>/dev/null; }
u_target=$(uid "$tmp/target.yaml") u_other=$(uid "$tmp/other.yaml") u_alias=$(uid "$tmp/alias.yaml")

# C0: the control. An alias that does not answer as `other` would make every cell below prove nothing.
if [ -n "$u_target" ] && [ -n "$u_other" ] && [ "$u_target" != "$u_other" ] && [ "$u_alias" = "$u_other" ]; then
  ok "C0 the alias answers as other (kube-system uid), target is a different cluster"
else
  bad "C0 the alias is not a trap: target=[$u_target] other=[$u_other] alias=[$u_alias]"
  echo "kind target kubeconfig: $pass passed, $fail failed"; exit 1
fi
cur=$(kubectl --kubeconfig "$tmp/alias.yaml" config current-context)
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
else bad "A1 rc=$rc, in target: $(in_cluster "$tmp/target.yaml" && echo yes || echo no), in other: $(in_cluster "$tmp/other.yaml" && echo yes || echo no)"; tail -5 "$tmp/run.log"; fi

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

# M1: the same probe on a Taskfile that does not point KUBECONFIG anywhere must be red. The judgement is
# aimed_right, the very one A1 passed; the trap has to catch the mutant or the A1 above proves nothing.
sed "s|^  KUBE_ENV: .*|  KUBE_ENV: ':'|" "$root/Taskfile.yml" > "$mutfile"
if ! grep -q "^  KUBE_ENV: ':'" "$mutfile"; then
  bad "M1 the mutant Taskfile was not made"
else
  reset_ns
  run "$mutfile" "$tmp/alias.yaml" || true
  if aimed_right; then bad "M1 the mutant (no KUBECONFIG prefix) still passes aimed_right: the probe cannot see the alias"
  elif in_cluster "$tmp/other.yaml"; then ok "M1 mutant without the prefix: the namespace lands in other → red as it must"
  else bad "M1 the mutant created the namespace nowhere: $(tail -3 "$tmp/run.log")"; fi
fi
reset_ns

# Judged on the whole run
[ "$(cksum < "$tmp/alias.yaml")" = "$alias_before" ] && ok "the alias kubeconfig is unchanged" || bad "the alias kubeconfig was written"
[ "$(cksum < "$user_kc" 2>/dev/null || echo absent)" = "$user_before" ] && ok "the caller's ~/.kube/config is unchanged" || bad "the caller's ~/.kube/config was written"
# no kubectl call ran on the default kubeconfig: each one had a KUBECONFIG inside this run's directory or the task's own
# bin/kind/kubeconfig-<target>, or named its own --kubeconfig; and there were calls to judge (an empty log proves nothing)
calls=$(grep -c . "$tmp/kubectl.calls" 2>/dev/null || echo 0)
stray=$(grep -v -E "^($tmp/[^|:]*|$root/bin/kind/kubeconfig-$target)\|" "$tmp/kubectl.calls" | grep -v -E -- '--kubeconfig[ =]' | head -3 | tr '\n' ';')
if [ "$calls" -ge 10 ] && [ -z "$stray" ]; then ok "all $calls kubectl calls used a kubeconfig of this run (none on ~/.kube/config)"
else bad "kubectl calls: $calls, on another kubeconfig: [$stray]"; fi
kind get clusters 2>/dev/null | sort > "$tmp/clusters.after"
missing=$(comm -23 "$tmp/clusters.before" "$tmp/clusters.after" | tr '\n' ' ')
[ -z "$missing" ] && ok "every cluster that existed before is still there" || bad "clusters gone: $missing"

echo "kind target kubeconfig: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
