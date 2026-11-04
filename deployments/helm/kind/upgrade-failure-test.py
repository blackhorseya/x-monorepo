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


def run_scenario(script, mode, pg, flow=None, create_reply_fails=False, refused=False, preexisting_ns=False):
    """Run `script` (payment-upgrade.sh or a mutant) behind the guard and return the problems found.
    mode: "install" (the previous chart does not install) or "upgrade" (an order cannot be placed during the upgrade)."""
    root = pathlib.Path(tempfile.mkdtemp(prefix="upgrade-failure-test-"))
    bindir, nsdir = root / "bin", root / "namespaces"
    bindir.mkdir()
    nsdir.mkdir()
    events = root / "events"
    # The home database holds something that is not the drill's. Planted fresh for every run.
    for sql in ("DROP SCHEMA IF EXISTS unrelated CASCADE", "DROP TABLE IF EXISTS public.drill_mutant",
                "CREATE SCHEMA unrelated", "CREATE TABLE unrelated.keep (v text)", f"INSERT INTO unrelated.keep VALUES ('{SENTINEL}')"):
        assert psql(pg, sql).returncode == 0, sql
    if preexisting_ns:  # someone else's namespace under the very name this run would take
        (nsdir / f"x-monorepo-upgrade-{RUN_ID}").write_text("someone-else")

    (bindir / "helm").write_text(f'''#!/bin/bash
if [ "$1" = template ]; then exec {helm_real} "$@"; fi
printf 'helm %s\\n' "$*" >> "$EVENTS"
[ "$1" = upgrade ] || exit 0
n=$(grep -c '^helm upgrade' "$EVENTS")
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
  "create -f -")
    m=$(cat)
    name=$(sed -n 's/^  name: //p' <<<"$m")
    lab=$(sed -n 's/^    x-monorepo-upgrade-drill: "\\(.*\\)"/\\1/p' <<<"$m")
    if [ -e "$NSDIR/$name" ]; then echo 'Error from server (AlreadyExists)' >&2; exit 1; fi
    echo "$lab" > "$NSDIR/$name"
    [ -z "$CREATE_REPLY_FAILS" ] || exit 1;;  # the create commits, then the answer is lost
  "get namespace "*)
    want=
    for a in "$@"; do case "$a" in *=*) want=${{a#*=}};; esac; done
    case "$3" in
      -*) ;;  # a list: all namespaces (matching the selector, if any)
      *) if [ -n "$want" ]; then echo 'error: name cannot be provided when a selector is specified' >&2; exit 1; fi  # as the real kubectl
         [ -e "$NSDIR/$3" ] || exit 1; echo "namespace/$3"; exit 0;;
    esac
    for f in "$NSDIR"/*; do
      [ -e "$f" ] || continue
      if [ -z "$want" ] || [ "$(cat "$f")" = "$want" ]; then echo "namespace/$(basename "$f")"; fi
    done;;
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
               ORDERS=str(root / "orders"), MODE=mode, PGC=pg, NSDIR=str(nsdir), HOME_NS=HOME_NS,
               POSTGRES_URL_CHECK=str(check_file), UPGRADE_SCRIPT=str(script),
               CREATE_REPLY_FAILS="1" if create_reply_fails else "")
    if preexisting_ns:
        env["UPGRADE_RUN_ID"] = RUN_ID
    cmd = ["bash", str(guard_file), HOME_NS, "rel", "deployments/helm/x-monorepo", str(values_file), str(flow or flow_file)]
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
        creates = [i for i, x in enumerate(lines) if x == "kubectl create -f -"]
        deletes = [(i, x.split()[3]) for i, x in enumerate(lines) if x.startswith("kubectl delete namespace ")]
        done = [i for i, x in enumerate(lines) if x == "HELM3_DONE"]
        left = sorted(f.name for f in nsdir.iterdir())

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
        if refused:
            # refused up front: nothing created, nothing installed, nothing deleted
            if creates or deletes or upgrades or left:
                problems.append(f"a refused run must create, install and delete nothing, got creates={len(creates)} deletes={deletes} upgrades={len(upgrades)} namespaces={left}")
            if "payment-upgrade: refused" not in outtxt:
                problems.append("the run was not refused up front (no 'payment-upgrade: refused' in its output)")
        elif preexisting_ns:
            if "refused: could not create the namespace" not in outtxt:
                problems.append("a namespace that already exists was not refused")
            if deletes or upgrades or left != [f"x-monorepo-upgrade-{RUN_ID}"]:
                problems.append(f"a namespace that was already there must be left alone and nothing installed: deletes={deletes} upgrades={len(upgrades)} namespaces={left}")
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


def main():
    pg = start_postgres()
    failed = False
    tmp = pathlib.Path(tempfile.mkdtemp(prefix="upgrade-failure-mutants-"))
    src = script_file.read_text()

    def mutant(name, old, new):
        assert old in src, f"the script changed shape: the {name} mutant cannot be built"
        f = tmp / f"{name}.sh"
        f.write_text(src.replace(old, new, 1))
        return f

    # a drill that writes to the home database / deletes the home namespace / skips the render check
    writes_home = mutant("writes-home", "ns_created=1\n", 'ns_created=1\nkubectl -n "$1" exec deploy/postgres -- psql -U app -d app -tAq -c "CREATE TABLE public.drill_mutant (x int)" >/dev/null\n')
    deletes_home = mutant("deletes-home", 'kubectl delete namespace "$ns"', 'kubectl delete namespace "$1"')
    no_check = mutant("no-check", "assert_connections() {", "assert_connections() { return 0;")

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

    try:
        for mode in ("install", "upgrade"):
            report(mode, run_scenario(script_file, mode, pg))
        # fault injection: the same checks must go red
        must_catch("install, writes-home", run_scenario(writes_home, "install", pg), "CHANGED", "unchanged", "names the home namespace", "sentinel")
        must_catch("upgrade, writes-home", run_scenario(writes_home, "upgrade", pg), "CHANGED", "unchanged", "names the home namespace", "sentinel")
        must_catch("install, deletes-home", run_scenario(deletes_home, "install", pg), "not the home one", "namespaces left behind", "names the home namespace")
        # the create commits and its answer is lost: the namespace is still cleaned up
        report("install, create reply lost", run_scenario(script_file, "install", pg, create_reply_fails=True))
        # a namespace that was already there is neither reused nor deleted
        report("namespace already exists", run_scenario(script_file, "install", pg, preexisting_ns=True))

        for name, override in REFUSALS.items():
            flow = tmp / ("flow-" + re.sub(r"\W", "_", name) + ".yaml")
            flow.write_text(yaml.safe_dump(merge(yaml.safe_load(flow_file.read_text()), override)))
            report(f"refused: {name}", run_scenario(script_file, "install", pg, flow=flow, refused=True))
            if name.startswith(("secrets.existingSecret", "rpc.extraEnv (FQDN of the home Postgres)", "config.postgres.url", "worker.extraEnv (FQDN of the home Redis)")):
                must_catch(f"refused: {name}, no-check", run_scenario(no_check, "install", pg, flow=flow, refused=True),
                           "must create", "not refused up front")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
        subprocess.run([rt, "rm", "-f", pg], capture_output=True)
    if failed:
        sys.exit(1)
    print("upgrade-failure-test: ok (home database unchanged on every path, the drill's namespace created and deleted, "
          f"{len(REFUSALS)} Postgres/Redis-source inputs refused up front, lost create reply cleaned up, an existing namespace left alone, "
          "background helm joined before the delete, migrate log printed, 3 mutants caught)")


main()
