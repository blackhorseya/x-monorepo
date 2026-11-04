#!/usr/bin/env bash
# Which cluster and namespace do the kind tasks aim at? (#430)  No kind, no kubectl, no helm is ever run.
#
#   deployments/helm/kind/target-test.sh      # needs task + go; `task helm-check` runs it
#
# The probe is the real entry, `task --dry <task> ...`: it expands every command the task would run and
# runs none of them. The judged value is what the expanded commands carry: `--name <cluster>` of kind,
# `-n <namespace>` of kubectl / helm, the namespace every kind/*.sh is handed first, the cluster
# `kind get clusters | grep -qx` looks for. Four sources, per task and per variable (CLUSTER, NS):
#
#   source                                   expected target
#   S1 nothing                               the default (x-monorepo)
#   S2 command line  VAR=probe               probe
#   S3 OS environment only  VAR=foo          the default: a stray CLUSTER / NS in a shell never re-aims the task
#   S4 both: environment foo + command line  the command line (probe)
#
# S3 is a decision, not a go-task default: go-task reads the environment as template variables, so a task
# that wrote `{{.CLUSTER | default "x-monorepo"}}` would follow `CLUSTER=foo`. The target of a task that
# creates, loads images into and installs on a cluster is named on the command line or not at all.
#
# Judged per cell: the set of values is exactly {expected} (one value, never a mix) and the number of
# places carrying it equals S1's (a cell that expands to nothing, or loses a command, is not green), and
# S1's count is at least the literal below (the anchor: the probe sees the commands it is supposed to).
#
# TASKFILE=<file in the repo root> runs the same cells against another Taskfile (default Taskfile.yml),
# to show them red on an older one: git show <old sha>:Taskfile.yml > Taskfile.old.yml (delete it afterwards).
set -uo pipefail
cd "$(dirname "$0")/../../.."

TASKFILE=${TASKFILE:-Taskfile.yml}
strip() { sed 's/\x1b\[[0-9;]*m//g'; }

tmp=$(mktemp -d) || exit 1
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
# Preconditions only ask `command -v kind kubectl helm jq k6`. jq stays real (the Taskfile's own vars run it);
# the stand-ins (kind, kubectl, helm, and k6 for helm-kind-keda's precondition: a CI runner has none of them)
# are first on PATH and any execution leaves a marker: a --dry that starts a tool is a failure of its own.
for b in kind kubectl helm k6; do
  printf '#!/bin/sh\necho "$0 $*" >> "%s/ran"\nexit 99\n' "$tmp" > "$tmp/bin/$b"
  chmod +x "$tmp/bin/$b"
done
export PATH="$tmp/bin:$PATH"

# Positive control for the tripwire: a stand-in that is run does leave the marker.
"$tmp/bin/kind" tripwire-self-test >/dev/null 2>&1
if ! grep -q tripwire-self-test "$tmp/ran" 2>/dev/null; then
  echo "FAIL tripwire: running a stand-in left no marker"; exit 1
fi
: > "$tmp/ran"

fail=0 pass=0
ok()  { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL $*"; }

# values_of <kind> <dry output file>: every value the expanded commands give the target of that kind.
#   cluster  `--name <v>` and `grep -qx <v>`          ns  ` -n <v>` and the first argument of kind/*.sh
#   kubeconfig  the file every kubectl / helm / kind/*.sh command is pointed at (`export KUBECONFIG=…/kubeconfig-<cluster>`);
#            a command that aims at nothing is counted as MISSING, so one that lost its prefix turns the cell red
#   kindkc   every `kind create|delete cluster` names its kubeconfig (`--kubeconfig …/kubeconfig-<cluster>`): without the flag kind
#            reads $KUBECONFIG, then ~/.kube/config, and `delete` removes the kind-<cluster> context from it (PR #435 r1)
#   kedans   the namespace KEDA itself is installed in (`helm upgrade --install keda … -n <v>`), not ns
values_of() {
  case $1 in
    cluster)
      { grep -oE -- '--name [^ ]+' "$2" | sed 's/^--name //'
        grep -oE -- 'grep -qx [^ ]+' "$2" | sed 's/^grep -qx //'; } ;;
    kubeconfig)
      sed 's/^task: \[[^]]*\] //' "$2" | grep -E '(^|[;| ])kubectl |(^|[;| ])helm .*(upgrade|status) |deployments/helm/kind/[a-z-]+\.sh ' | while IFS= read -r l; do
        case $l in
          *deployments/helm/kind/*.sh\ *)  # a script: behind KUBE_ENV, which exports the file and puts the shims on PATH
            case $l in
              *'export KUBECONFIG="'*'/deployments/helm/kind/shim:$PATH"'*) printf '%s\n' "$l" | grep -oE -- 'KIND_KUBECONFIG="[^"]*kubeconfig-[a-z0-9.-]+"' | head -1 | sed 's/.*kubeconfig-//; s/"$//' ;;
              *) echo MISSING ;;
            esac ;;
          *'--kubeconfig "'*)  # kubectl / helm: every one of them carries the flag (the count of flags must equal the count of tools)
            f=$(printf '%s\n' "$l" | grep -oE -- '--kubeconfig "[^"]*kubeconfig-[a-z0-9.-]+"' | sed 's/.*kubeconfig-//; s/"$//')
            t=$(printf '%s\n' "$l" | grep -oE -- '(^|[;| ])(kubectl|helm) ' | wc -l | tr -d ' ')
            [ "$(printf '%s\n' "$f" | grep -c .)" = "$t" ] && printf '%s\n' "$f" | sort -u | head -1 || echo MISSING ;;
          *) echo MISSING ;;
        esac
      done ;;
    kindkc)
      sed 's/^task: \[[^]]*\] //' "$2" | grep -E 'kind (create|delete) cluster' | while IFS= read -r l; do
        case $l in
          *'--kubeconfig "'*) printf '%s\n' "$l" | grep -oE -- '--kubeconfig "[^"]*kubeconfig-[a-z0-9.-]+"' | head -1 | sed 's/.*kubeconfig-//; s/"$//' ;;
          *) echo MISSING ;;
        esac
      done ;;
    kedans) grep -oE -- 'kedacore/keda -n [^ ]+' "$2" | sed 's/^kedacore\/keda -n //' ;;
    ns)
      { grep -v 'kedacore/keda' "$2" | grep -oE -- ' -n [^ ]+' | sed 's/^ -n //'
        grep -oE -- 'deployments/helm/kind/[a-z-]+\.sh [^ ]+' "$2" | awk '{print $2}'; } ;;
  esac
}

# run_cell <task> <kind: cluster|ns> <source: S1..S4> -> dry output in $tmp/out
run_cell() {
  local t=$1 k=$2 s=$3 var def
  case $k in cluster) var=CLUSTER ;; ns) var=NS ;; kubeconfig|kindkc) var=CLUSTER ;; kedans) var=KEDA_NS ;; esac
  case $s in
    S1) env -u CLUSTER -u NS -u KEDA_NS task -t "$TASKFILE" --dry "$t" ;;
    S2) env -u CLUSTER -u NS -u KEDA_NS task -t "$TASKFILE" --dry "$t" "$var=probe" ;;
    S3) env -u CLUSTER -u NS -u KEDA_NS "$var=foo" task -t "$TASKFILE" --dry "$t" ;;
    S4) env -u CLUSTER -u NS -u KEDA_NS "$var=foo" task -t "$TASKFILE" --dry "$t" "$var=probe" ;;
  esac 2>&1 | strip > "$tmp/out"
  return "${PIPESTATUS[0]}"
}

# task, kind (cluster / ns / kubeconfig / kindkc / kedans), literal minimum number of places in S1 (the anchor)
cells=(
  "helm-kind-up cluster 3"
  "helm-kind-up ns 5"
  "helm-kind cluster 3"
  "helm-kind ns 15"
  "helm-kind-keda cluster 3"
  "helm-kind-keda ns 8"
  "helm-kind-keda kedans 1"
  "helm-kind-down cluster 1"
  "helm-kind-up kindkc 1"
  "helm-kind-down kindkc 1"
  "helm-kind-up kubeconfig 6"
  "helm-kind kubeconfig 16"
  "helm-kind-keda kubeconfig 11"
)
for c in "${cells[@]}"; do
  read -r t k min <<<"$c"
  base=""
  for s in S1 S2 S3 S4; do
    case $s in S1|S3) want=x-monorepo; [ "$k" = kedans ] && want=keda ;; S2|S4) want=probe ;; esac
    id="$t $k $s"
    if ! run_cell "$t" "$k" "$s"; then
      bad "$id: task --dry exited non-zero: $(head -3 "$tmp/out" | tr '\n' '|')"
      continue
    fi
    values_of "$k" "$tmp/out" > "$tmp/vals"
    n=$(grep -c . "$tmp/vals" || true)
    got=$(sort -u "$tmp/vals" | tr '\n' ' ')
    [ "$s" = S1 ] && base=$n
    if [ "$got" != "$want " ]; then
      bad "$id: target is [${got% }], want [$want]"
    elif [ "$s" = S1 ] && [ "$n" -lt "$min" ]; then
      bad "$id: only $n places carry the target, want at least $min (the probe is not seeing the commands)"
    elif [ "$n" != "$base" ]; then
      bad "$id: $n places carry the target, S1 has $base"
    else
      ok
    fi
  done
done

# A name that is not a plain lowercase DNS-ish name is refused before any command is expanded (the values are
# interpolated into shell commands): non-zero exit, the reason on stderr, no `--name` / `-n` / kind/*.sh line.
for t in helm-kind-up helm-kind helm-kind-keda helm-kind-down; do
  for arg in 'CLUSTER=a b' 'CLUSTER=x;touch pwned' 'CLUSTER=UPPER' 'CLUSTER=' 'NS=-x' 'NS=a$(id)' 'KEDA_NS=k_s'; do
    id="$t refuses $arg"
    if env -u CLUSTER -u NS -u KEDA_NS task -t "$TASKFILE" --dry "$t" "$arg" 2>&1 | strip > "$tmp/out"; [ "${PIPESTATUS[0]}" -eq 0 ]; then
      bad "$id: exited 0"
    elif ! grep -q 'must be lowercase letters' "$tmp/out"; then
      bad "$id: failed for another reason: $(head -2 "$tmp/out" | tr '\n' '|')"
    elif grep -qE -- '--name|^task: \[[^]]+\] (kubectl|helm|kind|deployments/helm/kind)' "$tmp/out"; then
      bad "$id: commands were expanded before the refusal"
    else
      ok
    fi
  done
done
[ -e pwned ] && { bad "a refused name still ran a command (./pwned exists)"; rm -f pwned; }

if [ -s "$tmp/ran" ]; then
  bad "a tool ran under --dry: $(head -3 "$tmp/ran" | tr '\n' '|')"
else
  ok
fi

echo "kind target: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
