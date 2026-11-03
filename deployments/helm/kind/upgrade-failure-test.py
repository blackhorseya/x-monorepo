#!/usr/bin/env python3
"""payment-upgrade.sh owes the same thing whatever happens: the release is restored LAST (#422).

Stage 4 of the script upgrades the release with helm in the background while it places orders. If an
order cannot be placed the script exits through its EXIT trap, which restores the release. The restore
must not race the background helm: if the trap restored first and the helm upgrade finished after, the
release would end in the upgraded-with-overlay state, not the restored one, and the script's exit code
would not say so.

This drives the REAL payment-upgrade.sh with fake helm / kubectl / curl / sleep (no cluster):
  - fake helm: the 3rd `helm upgrade` is the slow background one (5 seconds, then it sets the release
    state to FLOW); every other `helm upgrade` is a restore/install (sets RESTORED). `helm template` is real.
  - fake curl: the first UpsertProduct after the background upgrade started answers 500, so order D1 fails.
Expected: the script exits non-zero (its exit code is kept), the background helm finished BEFORE the
restore ran, the release state ends RESTORED, and no process of the script's session is left.

  deployments/helm/kind/upgrade-failure-test.py      # task helm-check runs it; needs helm
"""
import os
import pathlib
import shutil
import signal
import subprocess
import sys
import tempfile
import time

REPO = pathlib.Path(__file__).resolve().parents[3]
root = pathlib.Path(tempfile.mkdtemp(prefix="upgrade-failure-test-"))
bindir = root / "bin"
bindir.mkdir()
events = root / "events"
# the values the real helm-kind passes: the chart must render with them (helm template is real)
values_file = REPO / "deployments/helm/kind/values.yaml"
flow_file = REPO / "deployments/helm/kind/payment-flow-values.yaml"
helm_real = shutil.which("helm")
assert helm_real, "helm is needed (helm template is real)"

(bindir / "helm").write_text(f'''#!/bin/bash
if [ "$1" = template ]; then exec {helm_real} "$@"; fi
printf 'helm upgrade %s\\n' "$*" >> "$EVENTS"
n=$(grep -c '^helm upgrade' "$EVENTS")
if [ "$n" = 3 ]; then
  : > "$UPGRADE_STARTED"
  /bin/sleep 5
  echo HELM3_DONE >> "$EVENTS"
  echo FLOW > "$RELEASE_STATE"
else
  echo RESTORED > "$RELEASE_STATE"
fi
''')
(bindir / "kubectl").write_text('''#!/bin/bash
printf 'kubectl %s\\n' "$*" >> "$EVENTS"
case "$*" in
  *"get deploy"*) echo 'ghcr.io/blackhorseya/x-monorepo/payment-rpc:0.63.3';;
  *" exec deploy/postgres "*)
    q=${*: -1}
    case "$q" in
      *"consumer_group, offset_acked"*)
        t=$(sed -n "s/.*SELECT '\\([^']*\\)'.*/\\1/p" <<<"$q")
        echo "$t|payment-rpc.payment|1";;
      *) echo 1;;
    esac;;
esac
''')
(bindir / "curl").write_text('''#!/bin/bash
case "$*" in *readyz*) exit 0;; esac
out=''
args=("$@")
for ((i=0;i<${#args[@]};i++)); do [ "${args[i]}" = '-o' ] && out=${args[i+1]}; done
case "$*" in
  *UpsertProduct*) if [ -e "$UPGRADE_STARTED" ]; then code=500; body='{}'; else code=200; body='{}'; fi;;
  *CreateOrder*) n=$(wc -l < "$ORDERS"); n=$((n+1)); echo "$n" >> "$ORDERS"; code=200; body="{\\"order\\":{\\"id\\":\\"ord-$n\\"}}";;
  *GetOrder*) code=200; body='{"order":{"status":"ORDER_STATUS_CONFIRMED"}}';;
  *GetPayment*) code=200; case "$*" in *ord-1*) body='{"payment":{"status":"PAYMENT_STATUS_PENDING"}}';; *) body='{}';; esac;;
  *) code=200; body='{}';;
esac
[ -z "$out" ] || printf '%s' "$body" > "$out"
printf 'HTTP/1.1 %s OK\\r\\n\\r\\n' "$code"
''')
(bindir / "sleep").write_text("#!/bin/bash\nexit 0\n")  # the script's own waits are not what is under test
for f in bindir.iterdir():
    f.chmod(0o755)
(root / "orders").write_text("")

env = os.environ.copy()
env.update(PATH=f"{bindir}:{env['PATH']}", EVENTS=str(events), UPGRADE_STARTED=str(root / "upgrade_started"),
           ORDERS=str(root / "orders"), RELEASE_STATE=str(root / "release_state"))
cmd = ["bash", "deployments/helm/kind/payment-upgrade.sh", "ns", "rel", "deployments/helm/x-monorepo",
       str(values_file), str(flow_file)]
out = open(root / "out", "wb")
p = subprocess.Popen(cmd, cwd=REPO, env=env, stdout=out, stderr=subprocess.STDOUT, start_new_session=True)
problems = []
try:
    try:
        rc = p.wait(timeout=60)
    except subprocess.TimeoutExpired:
        rc = None
        problems.append("the script did not exit within 60s")
    lines = events.read_text().splitlines() if events.exists() else []
    upgrades = [i for i, x in enumerate(lines) if x.startswith("helm upgrade")]
    done = [i for i, x in enumerate(lines) if x == "HELM3_DONE"]
    state = (root / "release_state").read_text().strip() if (root / "release_state").exists() else "none"
    if not (root / "upgrade_started").exists():
        problems.append("the premise failed: the background upgrade never started, so nothing was tested")
    if rc in (0, None):
        problems.append(f"exit code {rc}: a failed run must exit non-zero (the exit code is kept through the trap)")
    if len(upgrades) < 4:
        problems.append(f"{len(upgrades)} helm upgrades ran, want the old chart (x2), the background one and the restore (4)")
    elif not done:
        problems.append("the background helm upgrade never finished before the script exited")
    elif done[0] > upgrades[-1]:
        problems.append("the restore ran BEFORE the background helm upgrade finished: the upgrade overwrote it afterwards")
    if state != "RESTORED":
        problems.append(f"the release ended in state {state}, want RESTORED (the restore must be the last change)")
    # nothing of the script's session may outlive it (a child left behind keeps changing the cluster)
    time.sleep(0.2)
    try:
        os.killpg(p.pid, 0)
        problems.append("a process of the script's session is still alive after it exited")
    except ProcessLookupError:
        pass
finally:
    try:
        os.killpg(p.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    if p.poll() is None:
        p.wait()
    out.close()

if problems:
    print("upgrade-failure-test: FAIL", file=sys.stderr)
    for x in problems:
        print("  - " + x, file=sys.stderr)
    print((root / "out").read_text(errors="replace")[-600:], file=sys.stderr)
    sys.exit(1)
shutil.rmtree(root, ignore_errors=True)
print("upgrade-failure-test: ok (exit code kept, background helm joined before the restore, release RESTORED, no child left)")
