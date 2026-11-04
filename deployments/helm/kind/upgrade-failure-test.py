#!/usr/bin/env python3
"""payment-upgrade.sh owes the same things whatever happens (#422, #427, #429).

(1) It never touches the home release. `task helm-kind` reuses an existing kind cluster, and the Postgres of the home
namespace keeps its data between runs, so it may hold things that are not this test's. The drill therefore runs in a
namespace of its own with its own Postgres and Redis and its own release. payment-upgrade-guard.sh (what `task helm-kind`
runs, CI included) snapshots the home database read-only before and after and requires it unchanged. Here the home
database is a REAL Postgres (a docker/podman container, psql runs inside it) holding an unrelated schema with a sentinel row.
(2) It is refused up front, before anything is created, when a chart input would make a workload connect somewhere other
than the drill's own Postgres / Redis: every existingSecret, every extraEnv, the config file, a fully qualified name that
points at the home namespace (checked on the RENDERED chart, postgres-url-check.py).
(3) It deletes the namespace it created, and only that one, whatever happens: after a failed install, after a failure while
the upgrade is still rolling (the background helm is joined first, #422), after a create whose answer was lost; a namespace
that was already there under the name is not reused and not touched.
(4) A failure prints the logs of the migrate hook Job.
(5) Every input is validated before anything is created or sent (#432): the two ports go into http://127.0.0.1:<port> and curl
sends the X-API-Key there, so a value like 18096@127.0.0.1:<other port> must be refused. That gate uses the REAL curl, a synthetic
key and loopback receivers, and asserts that no receiver saw any request (so no key).
(6) A SIGTERM between the create being sent and its uid being kept leaves the namespace in place, deletes nothing, and says what
to delete by hand (#433).

This drives the REAL payment-upgrade-guard.sh -> payment-upgrade.sh with fake helm / kubectl / curl / sleep (no cluster):
  - fake helm: the 3rd `helm upgrade` is the slow background one (5 seconds); `helm template` is real. In the "install"
    scenario the first `helm upgrade` (the previous chart) fails.
  - fake curl: the first UpsertProduct after the background upgrade started answers 500, so order D1 fails.
  - fake kubectl: keeps a set of namespaces; statements for the HOME namespace go to the real Postgres, the drill's are canned.
The same checks run against copies of the script with a fault put in (writes to the home database; deletes the home
namespace; no render check) and MUST fail: that is what shows the assertions can go red.

  deployments/helm/kind/upgrade-failure-test.py      # task helm-check runs it; needs helm and docker or podman
"""
import os
import pathlib
import re
import shutil
import signal
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
import subprocess
import sys
import tempfile
import time
import uuid

import yaml

REPO = pathlib.Path(__file__).resolve().parents[3]
values_file = REPO / "deployments/helm/kind/values.yaml"
flow_file = REPO / "deployments/helm/kind/payment-flow-values.yaml"
script_file = REPO / "deployments/helm/kind/payment-upgrade.sh"
check_file = REPO / "deployments/helm/kind/postgres-url-check.py"
guard_file = REPO / "deployments/helm/kind/payment-upgrade-guard.sh"  # what `task helm-kind` runs
helm_real = shutil.which("helm")
assert helm_real, "helm is needed (helm template is real)"
rt = shutil.which("docker") or shutil.which("podman")
assert rt, "docker or podman is needed (a real Postgres is the home database)"
HOME_NS = "ns"  # the namespace argument the guard is given
SENTINEL = "keep-me-" + uuid.uuid4().hex[:8]
RUN_ID = "1700000000-4242"  # fixed for the "namespace already exists" cell


def psql(container, sql):
    return subprocess.run([rt, "exec", container, "psql", "-U", "app", "-d", "app", "-tAq", "-c", sql], capture_output=True, text=True)


def start_postgres():
    name = "upgrade-failure-test-" + uuid.uuid4().hex[:8]
    subprocess.run([rt, "run", "-d", "--name", name, "-e", "POSTGRES_USER=app", "-e", "POSTGRES_PASSWORD=app",
                    "-e", "POSTGRES_DB=app", "postgres:16-alpine"], check=True, capture_output=True)
    ok = 0
    for _ in range(90):  # the image starts a throwaway server for its init, stops it, then starts the real one
        r = psql(name, "select 1")
        ok = ok + 1 if r.returncode == 0 and r.stdout.strip() == "1" else 0
        if ok >= 3:
            return name
        time.sleep(1)
    subprocess.run([rt, "rm", "-f", name], capture_output=True)
    raise SystemExit("upgrade-failure-test: the Postgres container never became ready")


class Receiver:
    """A loopback HTTP server that records every request it gets (path, Host, X-API-Key) and answers 200 {}."""

    def __init__(self):
        self.hits = []
        hits = self.hits

        class H(BaseHTTPRequestHandler):
            def handle_any(self):
                n = int(self.headers.get("Content-Length") or 0)
                if n:
                    self.rfile.read(n)
                hits.append((self.path, self.headers.get("Host"), self.headers.get("X-API-Key")))
                self.send_response(200)
                self.send_header("Content-Length", "2")
                self.end_headers()
                self.wfile.write(b"{}")

            do_GET = do_POST = handle_any

            def log_message(self, *a):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), H)
        self.port = self.server.server_address[1]
        # fire-and-forget: test fixture, stopped by stop()
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def stop(self):
        self.server.shutdown()
        self.server.server_close()


KILL_SCRIPT = """#!/bin/bash
# SIGTERM the payment-upgrade.sh that is running this fake: the topmost ancestor whose command line is that script
p=$PPID
while q=$(ps -o ppid= -p "$p" | tr -d ' '); ps -o args= -p "$q" 2>/dev/null | grep -q 'kind/payment-upgrade.sh'; do p=$q; done
kill -TERM "$p"
"""


def run_scenario(script, mode, pg, flow=None, create_reply_fails=False, refused=False, preexisting_label=None, inherited=None, swapped=False,
                 env_extra=None, recv=None, positive=False, sig=None):
    """Run `script` (payment-upgrade.sh or a mutant, in a directory of its own next to the real guard, checker and deps.yaml) behind
    the guard and return the problems found. `inherited`: environment variables a parent exported (internal state of the script).
    mode: "install" (the previous chart does not install) or "upgrade" (an order cannot be placed during the upgrade).
    `recv`: loopback receivers; given, the REAL curl is used (no fake) and the key in `flow` reaches whoever curl really connects to.
    `sig`: "before-commit" | "after-reply" | "after-save": SIGTERM the script at that point of the namespace create (#433)."""
    for r in recv or []:
        r.hits.clear()
    root = pathlib.Path(tempfile.mkdtemp(prefix="upgrade-failure-test-"))
    bindir, nsdir, reldir = root / "bin", root / "namespaces", root / "releases"
    bindir.mkdir()
    nsdir.mkdir()
    reldir.mkdir()
    events = root / "events"
    # The home database holds something that is not the drill's. Planted fresh for every run.
    for sql in ("DROP SCHEMA IF EXISTS unrelated CASCADE", "DROP TABLE IF EXISTS public.drill_mutant",
                "CREATE SCHEMA unrelated", "CREATE TABLE unrelated.keep (v text)", f"INSERT INTO unrelated.keep VALUES ('{SENTINEL}')"):
        assert psql(pg, sql).returncode == 0, sql
    (nsdir / HOME_NS).write_text("home\nuid-home\n")  # the home release's namespace, in the fake cluster
    if preexisting_label is not None:  # a namespace that is already there under the very name this run would take
        (nsdir / f"x-monorepo-upgrade-{RUN_ID}").write_text(preexisting_label + "\n11111111-1111-4111-8111-111111111111\n")
        (reldir / f"x-monorepo-upgrade-{RUN_ID}").write_text("someone else's release")

    kdir = root / "kind"
    kdir.mkdir()
    shutil.copy(guard_file, kdir / "payment-upgrade-guard.sh")
    shutil.copy(check_file, kdir / "postgres-url-check.py")
    shutil.copy(guard_file.parent / "deps.yaml", kdir / "deps.yaml")
    shutil.copy(script, kdir / "payment-upgrade.sh")
    (bindir / "helm").write_text(f'''#!/bin/bash
if [ "$1" = template ]; then exec {helm_real} "$@"; fi
printf 'helm %s\\n' "$*" >> "$EVENTS"
if [ "$1" = uninstall ]; then rm -f "$RELDIR/$(sed -n 's/.* -n \\([^ ]*\\).*/\\1/p' <<<"$*")"; exit 0; fi
[ "$1" = upgrade ] || exit 0
n=$(grep -c '^helm upgrade' "$EVENTS")
if [ -n "$SWAPPED" ] && [ "$n" = 1 ]; then  # before cleanup: the namespace is deleted and re-made under the same name AND the same token label
  for f in "$NSDIR"/x-monorepo-upgrade-*; do printf '%s\\n22222222-2222-4222-8222-222222222222\\n' "$(head -1 "$f")" > "$f"; echo "someone else's new release" > "$RELDIR/$(basename "$f")"; done
fi
if [ "$MODE" = install ] && [ "$n" = 1 ]; then exit 1; fi  # the previous chart does not install
if [ "$MODE" = upgrade ] && [ "$n" = 3 ]; then
  : > "$UPGRADE_STARTED"
  /bin/sleep 5
  echo HELM3_DONE >> "$EVENTS"
fi
''')
    (bindir / "kubectl").write_text(f'''#!/bin/bash
printf 'kubectl %s\\n' "$*" >> "$EVENTS"
case "$*" in
  "create -f - -o jsonpath="*)
    m=$(cat)
    name=$(sed -n 's/^  name: //p' <<<"$m")
    lab=$(sed -n 's/^    x-monorepo-upgrade-drill: "\\(.*\\)"/\\1/p' <<<"$m")
    if [ -e "$NSDIR/$name" ]; then echo 'Error from server (AlreadyExists)' >&2; exit 1; fi
    if [ "$SIG" = before-commit ]; then "$KILLSCRIPT"; exit 1; fi  # killed before the create is applied
    uid=$(printf '%08x-aaaa-4bbb-8ccc-%012x' $((RANDOM*32768+RANDOM)) $((RANDOM*32768+RANDOM)))
    printf '%s\\n%s\\n' "$lab" "$uid" > "$NSDIR/$name"
    [ -z "$CREATE_REPLY_FAILS" ] || exit 1  # the create commits, then the answer is lost
    printf '%s' "$uid"
    if [ "$SIG" = after-reply ]; then "$KILLSCRIPT"; fi;;  # the answer is out; the script is killed before it keeps the uid
  *" apply -f "*) if [ "$SIG" = after-save ]; then "$KILLSCRIPT"; fi;;  # the uid has been kept
  "get namespace "*)
    want=
    prev=
    for a in "$@"; do [ "$prev" = -l ] && want=${{a#*=}}; prev=$a; done
    case "$3" in
      -*) ;;  # a list: all namespaces (matching the selector, if any)
      *) if [ -n "$want" ]; then echo 'error: name cannot be provided when a selector is specified' >&2; exit 1; fi  # as the real kubectl
         [ -e "$NSDIR/$3" ] || exit 1; echo "namespace/$3"; exit 0;;
    esac
    for f in "$NSDIR"/*; do
      [ -e "$f" ] || continue
      if [ -z "$want" ] || [ "$(head -1 "$f")" = "$want" ]; then
        case "$*" in
          *jsonpath*)
            echo "$(basename "$f") $(sed -n 2p "$f")";;
          *) echo "namespace/$(basename "$f")";;
        esac
      fi
    done;;
  "delete --raw /api/v1/namespaces/"*" -f -")
    name=${{3##*/}}
    body=$(cat)
    uid=$(sed -n 's/.*"uid":"\\([^"]*\\)".*/\\1/p' <<<"$body")
    [ -e "$NSDIR/$name" ] || {{ echo 'Error from server (NotFound)' >&2; exit 1; }}
    if [ -n "$uid" ] && [ "$uid" != "$(sed -n 2p "$NSDIR/$name")" ]; then echo 'Error from server (Conflict): Precondition failed: UID in precondition' >&2; exit 1; fi
    rm -f "$NSDIR/$name";;
  "delete namespace "*) rm -f "$NSDIR/$3";;
  "get deploy"*|*" get deploy"*) echo 'ghcr.io/blackhorseya/x-monorepo/payment-rpc:0.63.3';;
  *" exec deploy/postgres "*)
    q=${{*: -1}}
    ns=
    prev=
    for a in "$@"; do [ "$prev" = -n ] && [ -z "$ns" ] && ns=$a; prev=$a; done
    if [ "$ns" = "$HOME_NS" ]; then
      {rt} exec "$PGC" psql -U app -d app -tAq -c "$q"   # the home database: real, and read by the guard only
    else
      case "$q" in
        *"consumer_group, offset_acked"*)
          t=$(sed -n "s/.*SELECT '\\\\([^']*\\\\)'.*/\\\\1/p" <<<"$q")
          echo "$t|payment-rpc.payment|1";;
        *) echo 1;;
      esac
    fi;;
esac
''')
    (bindir / "killscript").write_text(KILL_SCRIPT)
    if recv is None:
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
    runner_home = root / "runner-home"  # stands in for run.sh's scratch HOME: the scripts refuse to start without it (#444)
    runner_home.mkdir()
    (runner_home / ".kind-runner").write_text("")
    real_home = env.get("HOME", "")
    env.setdefault("XDG_CONFIG_HOME", real_home + "/.config")
    env.setdefault("XDG_DATA_HOME", real_home + "/.local/share")
    env.update(HOME=str(runner_home), KIND_RUNNER_HOME=str(runner_home))
    env.update(PATH=f"{bindir}:{env['PATH']}", EVENTS=str(events), UPGRADE_STARTED=str(root / "upgrade_started"),
               ORDERS=str(root / "orders"), MODE=mode, PGC=pg, NSDIR=str(nsdir), RELDIR=str(reldir), HOME_NS=HOME_NS,
               CREATE_REPLY_FAILS="1" if create_reply_fails else "", SWAPPED="1" if swapped else "",
               SIG=sig or "", KILLSCRIPT=str(bindir / "killscript"))
    # No cluster is ever reachable from here: kubectl and helm are fakes first in PATH, and whatever default kubeconfig the machine has
    # (~/.kube/config may point at a real environment) is replaced by a file that does not exist, so a call that slipped past the
    # fakes would fail instead of reaching a context.
    env.update(KUBECONFIG=str(root / "no-such-kubeconfig"))
    env.pop("KUBE_CONTEXT", None)
    env.update(inherited or {})
    env.update(env_extra or {})
    if preexisting_label is not None:
        env["UPGRADE_RUN_ID"] = RUN_ID  # the caller may name the run, so a label made of the run id proves nothing
    cmd = ["bash", str(kdir / "payment-upgrade-guard.sh"), HOME_NS, "rel", "deployments/helm/x-monorepo", str(values_file), str(flow or flow_file)]
    out = open(root / "out", "wb")
    p = subprocess.Popen(cmd, cwd=REPO, env=env, stdout=out, stderr=subprocess.STDOUT, start_new_session=True)
    problems = []
    try:
        try:
            rc = p.wait(timeout=90)
        except subprocess.TimeoutExpired:
            rc = None
            problems.append("the script did not exit within 90s")
        lines = events.read_text().splitlines() if events.exists() else []
        outtxt = (root / "out").read_text(errors="replace")
        upgrades = [i for i, x in enumerate(lines) if x.startswith("helm upgrade")]
        creates = [i for i, x in enumerate(lines) if x.startswith("kubectl create -f -")]
        deletes = [(i, x.split()[3].rsplit("/", 1)[1]) for i, x in enumerate(lines) if x.startswith("kubectl delete --raw /api/v1/namespaces/")]
        plain_deletes = [x for x in lines if x.startswith("kubectl delete namespace ")]
        done = [i for i, x in enumerate(lines) if x == "HELM3_DONE"]
        left = sorted(f.name for f in nsdir.iterdir() if f.name != HOME_NS)
        if not (nsdir / HOME_NS).exists():
            problems.append("the home namespace was deleted")
        if plain_deletes:
            problems.append(f"a namespace was deleted by name, not by uid: {plain_deletes}")

        if rc in (0, None):
            problems.append(f"exit code {rc}: a failed run must exit non-zero (the exit code is kept through the trap)")
        # ---- the home release is not touched (#427, #429) ----
        if "guard: the home release's database is unchanged" not in outtxt:
            problems.append("the guard did not report the home database unchanged")
        got = psql(pg, "SELECT v FROM unrelated.keep")
        if got.returncode != 0 or got.stdout.strip() != SENTINEL:
            problems.append(f"the unrelated sentinel in the home database is gone or changed: {got.stdout.strip()!r} {got.stderr.strip()[:100]}")
        # nothing but the guard's read-only statements may name the home namespace, in any kubectl or helm call
        for x in lines:
            if re.search(rf"(^| )-n {HOME_NS}( |$)", x) and not re.fullmatch(
                    rf"kubectl -n {HOME_NS} exec deploy/postgres -- psql -U app -d app -tAq -c SET default_transaction_read_only = on; .*", x):
                problems.append(f"a call names the home namespace: {x}")
        if recv and not positive:
            seen = [(i, h) for i, r in enumerate(recv) for h in r.hits]
            if seen:
                problems.append(f"a request reached a loopback receiver (the key goes with it): {seen[:3]}")
        if sig:
            # #433: killed around the create. The namespace is never deleted unless its uid was kept; when it may exist and its uid
            # is not known, the output names it and its label so a human can delete it.
            existing = [f for f in nsdir.iterdir() if f.name != HOME_NS]
            label = existing[0].read_text().splitlines()[0] if existing else None
            if sig == "after-save":
                if left or len(deletes) != 1:
                    problems.append(f"killed after the uid was kept: the namespace must be deleted by uid, left={left} deletes={deletes}")
                if "interrupted before the uid" in outtxt:
                    problems.append("a hint to delete by hand was printed although the uid was kept and the namespace deleted")
            else:
                if deletes or upgrades:
                    problems.append(f"killed before the uid was kept: nothing may be deleted or installed, deletes={deletes} upgrades={len(upgrades)}")
                if "interrupted before the uid" not in outtxt or f"namespace x-monorepo-upgrade-" not in outtxt or "x-monorepo-upgrade-drill=" not in outtxt:
                    problems.append("the interrupted run did not say which namespace and label to delete by hand")
                if sig == "after-reply":
                    if len(left) != 1:
                        problems.append(f"the committed namespace must be left in place: {left}")
                    elif f"x-monorepo-upgrade-drill={label}" not in outtxt or left[0] not in outtxt:
                        problems.append(f"the hint does not carry the namespace {left} and its label {label}")
                elif left:
                    problems.append(f"nothing was created, yet a namespace is there: {left}")
        elif positive:
            # a legitimate custom port: accepted, and the key goes to that port on 127.0.0.1 and nowhere else
            if "payment-upgrade: refused" in outtxt:
                problems.append("legitimate custom ports were refused")
            rpc_hits = [h for h in recv[0].hits if h[2]]
            if not rpc_hits or any(h[1] != f"127.0.0.1:{recv[0].port}" for h in recv[0].hits):
                problems.append(f"the key did not reach the chosen rpc port on 127.0.0.1: {recv[0].hits[:3]}")
            if not recv[1].hits:
                problems.append("the chosen serve port was not used")
            if left:
                problems.append(f"namespaces left behind: {left}")
        elif refused:
            # refused up front: nothing created, nothing installed, nothing deleted
            if creates or deletes or upgrades or left:
                problems.append(f"a refused run must create, install and delete nothing, got creates={len(creates)} deletes={deletes} upgrades={len(upgrades)} namespaces={left}")
            if "payment-upgrade: refused" not in outtxt:
                problems.append("the run was not refused up front (no 'payment-upgrade: refused' in its output)")
        elif swapped:
            # the namespace was deleted and re-made under the same name AND the same token label before cleanup looked at it: the
            # uid taken from the create's own answer no longer matches, so the replacement and its release must survive (409)
            if len(left) != 1 or (nsdir / left[0]).read_text().splitlines()[1] != "22222222-2222-4222-8222-222222222222":
                problems.append(f"the replacement namespace was deleted or changed: {left}")
            elif not (reldir / left[0]).exists():
                problems.append("the release of the replacement namespace is gone")
            if "left the namespace" not in outtxt:
                problems.append("a refused (409) delete was not reported")
        elif create_reply_fails:
            # the create was applied but its answer (and so its uid) was lost: nothing is adopted, installed or deleted, and the
            # output says what to delete by hand
            if len(creates) != 1 or deletes or upgrades or len(left) != 1:
                problems.append(f"a lost create answer must leave the namespace in place and install nothing: creates={len(creates)} deletes={deletes} upgrades={len(upgrades)} namespaces={left}")
            if "left in place, delete it by hand" not in outtxt or "x-monorepo-upgrade-drill=" not in outtxt:
                problems.append("a lost create answer did not report the namespace and its label for manual cleanup")
        elif preexisting_label is not None:
            # already there, whatever its label: refused, not used, not written to, not deleted
            theirs = f"x-monorepo-upgrade-{RUN_ID}"
            if "already exists" not in outtxt or "payment-upgrade: refused" not in outtxt:
                problems.append("a namespace that already exists was not refused")
            if deletes or upgrades or left != [theirs] or (nsdir / theirs).read_text().splitlines()[0] != preexisting_label:
                problems.append(f"a namespace that was already there must be left alone and nothing installed: deletes={deletes} upgrades={len(upgrades)} namespaces={left}")
            touched = [x for x in lines if f" -n {theirs} " in x or (x.startswith("helm ") and theirs in x)]
            if not (reldir / theirs).exists():
                problems.append("the release in the namespace that was already there was uninstalled")
            if any(x.startswith("helm uninstall") for x in lines):
                problems.append("helm uninstall ran although the namespace was not this run's")
            if touched:
                problems.append(f"something was run against the namespace that was already there: {touched}")
        else:
            # one namespace created, installed into, and deleted: that one, last
            if len(creates) != 1:
                problems.append(f"want exactly one namespace created, got {len(creates)}")
            if len(deletes) != 1 or (deletes and deletes[0][1] == HOME_NS):
                problems.append(f"want exactly one namespace deleted and not the home one, got {deletes}")
            if left:
                problems.append(f"namespaces left behind: {left}")
            if not any(x.startswith("kubectl -n x-monorepo-upgrade-") and "apply -f" in x and "deps.yaml" in x for x in lines):
                problems.append("the drill's own Postgres / Redis (deps.yaml) were not applied in its namespace")
            ups = [lines[i] for i in upgrades]
            if not ups or any(" -n x-monorepo-upgrade-" not in x or "paymentRpc.ingress.enabled=false" not in x or "secrets.postgresUrl" in x for x in ups):
                problems.append(f"every helm upgrade must run in the drill's namespace with the Ingress off: {ups}")
            if creates and upgrades and creates[0] > upgrades[0]:
                problems.append("a helm upgrade ran before the namespace was created")
            want_ups = 3 if mode == "upgrade" else 1
            if len(upgrades) != want_ups:
                problems.append(f"{len(upgrades)} helm upgrades ran, want {want_ups}")
            if mode == "upgrade":
                if not (root / "upgrade_started").exists():
                    problems.append("the premise failed: the background upgrade never started, so nothing was tested")
                elif not done:
                    problems.append("the background helm upgrade never finished before the script exited")
                elif deletes and done[0] > deletes[0][0]:
                    problems.append("the namespace was deleted BEFORE the background helm upgrade finished")
            # a failure must say why the migrate hook failed: the Job's log, by its job-name label
            if not any(x.startswith("kubectl -n x-monorepo-upgrade-") and " logs " in x and "job-name=x-monorepo-upg-" in x for x in lines):
                problems.append("the failure did not print the logs of the migrate Job of the drill's release")
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
        problems.append("script output (tail): " + (root / "out").read_text(errors="replace")[-500:].replace("\n", " | "))
    else:
        shutil.rmtree(root, ignore_errors=True)
    return problems


FQDN_PG = f"postgres://app:app@postgres.{HOME_NS}.svc.cluster.local:5432/app?sslmode=disable"  # the home release's Postgres
FQDN_REDIS = f"redis://redis.{HOME_NS}.svc.cluster.local:6379/0"
env_pg, env_redis = [{"name": "APP_POSTGRES_URL", "value": FQDN_PG}], [{"name": "APP_REDIS_URL", "value": FQDN_REDIS}]
# Every chart input that can change which Postgres / Redis a workload uses (swept from values.yaml and templates/: each
# existingSecret, each extraEnv, the config file). The drill must REFUSE each, before it creates or installs anything. The
# extraEnv of catalog-rpc and payment-rpc are refused by the chart itself at render time; the rest only the rendered-manifest
# check can see.
REFUSALS = {
    "secrets.existingSecret": {"secrets": {"existingSecret": "external-app-env"}},
    "secrets.catalogRpc.existingSecret": {"secrets": {"catalogRpc": {"existingSecret": "external-catalog-env", "apiKey": ""}}},
    "secrets.paymentRpc.existingSecret": {"secrets": {"paymentRpc": {"existingSecret": "external-payment-env", "apiKey": "", "mockWebhookSecret": ""}}},
    "rpc.extraEnv (FQDN of the home Postgres)": {"rpc": {"extraEnv": env_pg}},
    "worker.extraEnv (FQDN of the home Postgres)": {"worker": {"extraEnv": env_pg}},
    "worker.extraEnv (FQDN of the home Redis)": {"worker": {"extraEnv": env_redis}},
    "rpc.extraEnv (FQDN of the home Redis)": {"rpc": {"extraEnv": env_redis}},
    "console.extraEnv": {"console": {"extraEnv": env_pg}},
    "catalogRpc.extraEnv": {"catalogRpc": {"extraEnv": env_pg}},
    "paymentRpc.serve.extraEnv": {"paymentRpc": {"serve": {"extraEnv": env_pg}}},
    "paymentRpc.consume.extraEnv": {"paymentRpc": {"consume": {"extraEnv": env_pg}}},
    "paymentRpc.expire.extraEnv": {"paymentRpc": {"expire": {"extraEnv": env_pg}}},
    "config.postgres.url": {"config": {"postgres": {"url": FQDN_PG}}},
    "config.redis.url": {"config": {"redis": {"url": FQDN_REDIS}}},
}


def merge(a, b):
    out = dict(a)
    for k, v in b.items():
        out[k] = merge(out[k], v) if isinstance(v, dict) and isinstance(out.get(k), dict) else v
    return out


# Every environment variable the script reads. A new one fails the inventory check below until it is validated in the script
# (and listed here and in the PR description): that is what keeps the sweep from being a one-time thing (#432).
RUNNER_OWN = {"HOME", "KIND_RUNNER_HOME"}
ENV_INPUTS = {"UPGRADE_RUN_ID", "PAYMENT_UPGRADE_FROM_TAG", "PAYMENT_UPGRADE_FROM_IMAGE", "UPGRADE_RPC_PORT", "UPGRADE_SERVE_PORT", "RANDOM"}


def bad_port_values(target):
    """Values of a port variable that must be refused; `target` is the loopback port a successful injection would reach."""
    return {
        "userinfo@host:port": f"18096@127.0.0.1:{target}",
        "userinfo@host:port, trailing #": f"1@127.0.0.1:{target}#",
        "userinfo@host:port, trailing ?": f"1@127.0.0.1:{target}?",
        "host:port": f"127.0.0.1:{target}",
        "number:port": f"18096:{target}",
        "slash": "18096/x",
        "trailing space": "18096 ",
        "inner space": "18 096",
        "trailing newline": "18096\n",
        "newline then host": f"18096\n@127.0.0.1:{target}",
        "zero": "0",
        "above range": "65536",
        "huge": "99999999999999999999",
        "negative": "-1",
        "plus sign": "+18096",
        "empty": "",
        "leading zero": "018096",
        "leading zero, valid as octal": "01111",
        "hex": "0x4650",
        "fullwidth digits": "\uff11\uff18\uff10\uff19\uff16",
        "scientific": "1e4",
    }


ONLY = sys.argv[1:]  # label substrings: given, only the #432 / #433 cells whose label contains one of them run


def want(label):
    return not ONLY or any(o in label for o in ONLY)


def main():
    inputs_read = set(re.findall(r"\$\{?([A-Z][A-Z0-9_]+)", "\n".join(l for l in script_file.read_text().splitlines() if not l.lstrip().startswith("#"))))
    inputs_read -= RUNNER_OWN  # the refuse-outside-run.sh guard (#444) reads these two; run.sh sets them, an operator does not
    if inputs_read != ENV_INPUTS:
        print(f"upgrade-failure-test: FAIL: payment-upgrade.sh reads {sorted(inputs_read)}, the inventory is {sorted(ENV_INPUTS)}: "
              "validate the new input in the script, add a refusal cell here and list it in the PR", file=sys.stderr)
        sys.exit(1)
    pg = start_postgres()
    failed = False
    tmp = pathlib.Path(tempfile.mkdtemp(prefix="upgrade-failure-mutants-"))
    src = script_file.read_text()

    def mutant(name, old, new, *more):
        text = src
        for o, n in ((old, new),) + tuple(zip(more[::2], more[1::2])):
            if o not in text:  # the script changed shape: say so, and hand back the unchanged script (the mutant will then not be caught)
                nonlocal failed
                failed = True
                print(f"upgrade-failure-test: FAIL: the {name} mutant cannot be built ({o[:40]!r} is not in the script)", file=sys.stderr)
                continue
            text = text.replace(o, n, 1)
        f = tmp / f"{name}.sh"
        f.write_text(text)
        return f

    # a drill that writes to the home database / deletes the home namespace / skips the render check
    writes_home = mutant("writes-home", "ns_uid=$created\n", 'ns_uid=$created\nkubectl -n "$1" exec deploy/postgres -- psql -U app -d app -tAq -c "CREATE TABLE public.drill_mutant (x int)" >/dev/null\n')
    deletes_home = mutant("deletes-home", 'kubectl delete --raw "/api/v1/namespaces/$ns"', 'kubectl delete --raw "/api/v1/namespaces/ns"',
                          '''printf '{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":"%s"}}' "$ns_uid" |''',
                          '''printf '{"kind":"DeleteOptions","apiVersion":"v1"}' |''')
    # the delete is not bound to the uid that was looked up (r5: a namespace re-created under the same name is deleted)
    no_uid = mutant("no-uid-precondition", '''printf '{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":"%s"}}' "$ns_uid" |''',
                    '''printf '{"kind":"DeleteOptions","apiVersion":"v1"}' |''')
    # ownership by a label made of the caller's run id, AlreadyExists not refused, and a lost create answer adopted by looking the
    # namespace up by that label (r3 / r6): a namespace that was already there is installed into and deleted
    adopts = mutant("adopts-by-label",
                    'if kubectl get namespace "$ns" -o name >/dev/null 2>&1; then', "if false; then",
                    "token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \\n')", "token=$run_id",
                    'if grep -q AlreadyExists "$tmp/create.err"; then', "if false; then",
                    '    fail "refused: the create of $ns was applied',
                    '    created=$(kubectl get namespace -l "$run_label=$token" -o jsonpath=\'{.items[0].metadata.uid}\'); true || fail "refused: the create of $ns was applied')
    # internal state taken from the environment (the init line removed): what a parent exported steers cleanup()
    inherits = mutant("inherits-state", 'ns_uid="" token="" helm_pid="" soft="" pfs=""',
                      'ns_uid="${ns_uid:-}" token="${token:-}" helm_pid="${helm_pid:-}" soft="${soft:-}" pfs=""')
    # the uid is looked up at cleanup time (r6): whatever carries the token THEN is deleted, a re-made namespace included
    late_lookup = mutant("late-uid-lookup", '  if [ -n "$ns_uid" ]; then\n    printf \'{"kind"',
                         '''  ns_uid=$(kubectl get namespace -l "$run_label=$token" -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.uid}{"\\n"}{end}' | awk -v n="$ns" '$1 == n {print $2}')\n  if [ -n "$ns_uid" ]; then\n    printf '{"kind"''')
    no_check = mutant("no-check", "assert_connections() {", "assert_connections() { return 0;")
    # the ports go unchecked / the range or the leading-zero rule is dropped / the hint is not printed (#432, #433)
    no_port_check = mutant("no-port-check", "port_ok() {", "port_ok() { return 0;")
    no_range = mutant("no-port-range", ' && [ "$1" -le 65535 ]; }', "; }")
    zero_ok = mutant("leading-zero-ok", "^[1-9][0-9]{0,4}$", "^[0-9]{1,5}$")
    no_hint = mutant("no-interrupt-hint", 'if [ -n "$maybe_ns" ] && [ -z "$ns_uid" ]; then', "if false; then")
    no_flag = mutant("no-pending-flag", "maybe_ns=1 # from here", ": # from here")

    def report(label, problems):
        nonlocal failed
        if problems:
            failed = True
            print(f"upgrade-failure-test: FAIL ({label})", file=sys.stderr)
            for x in problems:
                print("  - " + x, file=sys.stderr)

    def must_catch(label, problems, *needles):
        nonlocal failed
        if not any(n in x for x in problems for n in needles):
            failed = True
            print(f"upgrade-failure-test: FAIL ({label}): the mutant was NOT caught: {problems}", file=sys.stderr)

    # #432: a synthetic key, never a real credential, in a copy of the flow values; two loopback receivers
    synth_shopper, synth_admin = "SYNTH-SHOPPER-" + uuid.uuid4().hex, "SYNTH-ADMIN-" + uuid.uuid4().hex
    synth_flow = tmp / "flow-synthetic-keys.yaml"
    flow_text = flow_file.read_text()
    assert "kind-shopper-key-0123456789ab" in flow_text and "kind-admin-key-0123456789abc" in flow_text
    synth_flow.write_text(flow_text.replace("kind-shopper-key-0123456789ab", synth_shopper).replace("kind-admin-key-0123456789abc", synth_admin))
    good, bad = Receiver(), Receiver()

    try:
        if not ONLY:
            for mode in ("install", "upgrade"):
                report(mode, run_scenario(script_file, mode, pg))
            # fault injection: the same checks must go red
            must_catch("install, writes-home", run_scenario(writes_home, "install", pg), "CHANGED", "unchanged", "names the home namespace", "sentinel")
            must_catch("upgrade, writes-home", run_scenario(writes_home, "upgrade", pg), "CHANGED", "unchanged", "names the home namespace", "sentinel")
            must_catch("install, deletes-home", run_scenario(deletes_home, "install", pg), "home namespace was deleted", "not the home one")
            # the create is applied and its answer is lost: its uid is unknown, so nothing is adopted or deleted; the run says what to delete
            report("install, create answer lost", run_scenario(script_file, "install", pg, create_reply_fails=True))
            # a namespace that was already there is neither reused nor written to nor deleted, whatever its label (r3: the caller can
            # name the run, so "same name, same label" is a state anyone can construct)
            for label_name, label in (("same label", RUN_ID), ("other label", "someone-else")):
                report(f"namespace already exists, {label_name}", run_scenario(script_file, "install", pg, preexisting_label=label))
                must_catch(f"namespace already exists, {label_name}, adopts-by-label", run_scenario(adopts, "install", pg, preexisting_label=label),
                           "already exists was not refused", "must be left alone", "something was run against") if label_name == "same label" else None

            # r6: deleted and re-made under the same name AND the same token label before cleanup runs: the uid from the create's answer no
            # longer matches, so the replacement and its release survive; a uid looked up at cleanup time (or no uid precondition) deletes it
            report("namespace replaced before cleanup", run_scenario(script_file, "install", pg, swapped=True))
            for m in (late_lookup, no_uid):
                must_catch(f"namespace replaced before cleanup, {m.stem}", run_scenario(m, "install", pg, swapped=True),
                           "replacement namespace was deleted", "release of the replacement")
            # r4: a parent that exported the script's internal state (ns_uid, token, ns, release...) next to a namespace of the same
            # name and label, with a release in it, must find both intact and no helm uninstall / kubectl delete at all
            state = {"ns_uid": "11111111-1111-4111-8111-111111111111", "token": "inherited-token", "release": "x-monorepo-upg-4242", "ns": f"x-monorepo-upgrade-{RUN_ID}",
                     "helm_pid": "1", "soft": "1", "run_label": "x-monorepo-upgrade-drill"}
            report("inherited internal state", run_scenario(script_file, "install", pg, preexisting_label="inherited-token", inherited=state))
            must_catch("inherited internal state, inherits-state", run_scenario(inherits, "install", pg, preexisting_label="inherited-token", inherited=state),
                       "left alone", "uninstall", "something was run against", "release in the namespace")

            for name, override in REFUSALS.items():
                flow = tmp / ("flow-" + re.sub(r"\W", "_", name) + ".yaml")
                flow.write_text(yaml.safe_dump(merge(yaml.safe_load(flow_file.read_text()), override)))
                report(f"refused: {name}", run_scenario(script_file, "install", pg, flow=flow, refused=True))
                if name.startswith(("secrets.existingSecret", "rpc.extraEnv (FQDN of the home Postgres)", "config.postgres.url", "worker.extraEnv (FQDN of the home Redis)")):
                    must_catch(f"refused: {name}, no-check", run_scenario(no_check, "install", pg, flow=flow, refused=True),
                               "must create", "not refused up front")

        # #432: every port value that is not a plain decimal 1-65535 is refused before any request is sent. Real curl, synthetic key,
        # loopback receivers: neither receiver may see anything. Both variables, every value.
        for var, other in (("UPGRADE_RPC_PORT", {"UPGRADE_SERVE_PORT": str(good.port)}), ("UPGRADE_SERVE_PORT", {"UPGRADE_RPC_PORT": str(good.port)})):
            for what, value in bad_port_values(bad.port).items():
                if want(f"refused: {var}={what}"):
                    report(f"refused: {var}={what}", run_scenario(script_file, "upgrade", pg, flow=synth_flow, refused=True, recv=[good, bad],
                                                              env_extra={**other, var: value}))
        for var, other in (("UPGRADE_RPC_PORT", {"UPGRADE_SERVE_PORT": str(good.port)}), ("UPGRADE_SERVE_PORT", {"UPGRADE_RPC_PORT": str(good.port)})):
            if not want("mutant-ports"):
                break
            bv = bad_port_values(bad.port)
            for m, what in ((no_port_check, "userinfo@host:port"), (no_port_check, "empty"), (no_range, "above range"), (zero_ok, "leading zero, valid as octal")):
                if m is no_port_check and what == "userinfo@host:port" and var == "UPGRADE_RPC_PORT":
                    probs = run_scenario(m, "upgrade", pg, flow=synth_flow, refused=True, recv=[good, bad], env_extra={**other, var: bv[what]})
                    must_catch(f"refused: {var}={what}, {m.stem}", probs, "a request reached a loopback receiver")
                    if not any(h[2] and h[2].startswith("SYNTH-") for h in bad.hits):
                        failed = True
                        print(f"upgrade-failure-test: FAIL: the unchecked port did not deliver the synthetic key to the receiver: {bad.hits[:3]}", file=sys.stderr)
                elif m is no_port_check and var == "UPGRADE_SERVE_PORT" and what == "userinfo@host:port":
                    must_catch(f"refused: {var}={what}, {m.stem}", run_scenario(m, "upgrade", pg, flow=synth_flow, refused=True, recv=[good, bad], env_extra={**other, var: bv[what]}),
                               "a request reached a loopback receiver")
                elif var == "UPGRADE_RPC_PORT" and m is not no_port_check:
                    must_catch(f"refused: {var}={what}, {m.stem}", run_scenario(m, "upgrade", pg, flow=synth_flow, refused=True, recv=[good, bad], env_extra={**other, var: bv[what]}),
                               "not refused up front", "must create")
        # a legitimate custom port pair still works, and the key goes to exactly that port
        if want("custom ports"):
            report("custom ports accepted", run_scenario(script_file, "upgrade", pg, flow=synth_flow, positive=True, recv=[good, bad],
                                                         env_extra={"UPGRADE_RPC_PORT": str(good.port), "UPGRADE_SERVE_PORT": str(bad.port)}))
        # #433: SIGTERM around the create, from the real entry
        for where in ("before-commit", "after-reply", "after-save"):
            if want(f"SIGTERM {where}"):
                report(f"SIGTERM {where}", run_scenario(script_file, "install", pg, sig=where))
        for m in (no_hint, no_flag) if want("mutant-sigterm") else ():
            for where in ("before-commit", "after-reply"):
                must_catch(f"SIGTERM {where}, {m.stem}", run_scenario(m, "install", pg, sig=where), "did not say which namespace", "does not carry")
    finally:
        good.stop()
        bad.stop()
        shutil.rmtree(tmp, ignore_errors=True)
        subprocess.run([rt, "rm", "-f", pg], capture_output=True)
    if failed:
        sys.exit(1)
    print("upgrade-failure-test: ok (home database unchanged on every path, the drill's namespace created and deleted, "
          f"{len(REFUSALS)} Postgres/Redis-source inputs refused up front, a lost create answer adopts and deletes nothing, an existing namespace (same or other label) left alone, "
          "background helm joined before the delete, migrate log printed, inherited internal state ignored, a namespace replaced before cleanup survives, "
          f"{2 * len(bad_port_values(0))} port values refused before any request (real curl, synthetic key, loopback receivers), custom ports accepted, "
          "SIGTERM around the create names the namespace and label and deletes nothing unless the uid was kept, 12 mutants caught)")


main()
