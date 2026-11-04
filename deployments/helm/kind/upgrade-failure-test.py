#!/usr/bin/env python3
"""payment-upgrade.sh owes the same things whatever happens.

(1) The release is restored LAST (#422). Stage 4 of the script upgrades the release with helm in the background while
it places orders. If an order cannot be placed the script exits through its EXIT trap, which restores the release. The
restore must not race the background helm: if the trap restored first and the helm upgrade finished after, the release
would end in the upgraded-with-overlay state, not the restored one, and the script's exit code would not say so.

(2) It never modifies anything that is not its own (#427, #429). `task helm-kind` reuses an existing kind cluster, and
its Postgres keeps its data between runs, so the release's own database ("app") may hold things that are not this
test's. Stage 1 therefore runs the previous chart on a database the script creates for itself and drops at the end; the
only statements it may send to "app" are that CREATE DATABASE and that DROP DATABASE. Checked on a REAL Postgres (a
docker/podman container; psql runs inside it): an unrelated schema with a sentinel row is planted in "app" first, the
script is driven to fail at two different points (the previous chart does not install; an order cannot be placed during
the upgrade), and afterwards the sentinel is there, no per-run database is left, and every statement sent to "app" is
one of the two allowed. The same check run against a copy of the script with a "drop every schema" step put back (the
version that was rejected in review) MUST fail: that is what shows the assertions can go red.

The entry point is payment-upgrade-guard.sh, the wrapper `task helm-kind` runs (CI included): it writes an unrelated
schema with a row to "app" before the drill and checks, after a success or a failure alike, that the row is there and
no upgrade_* database is left. On the success path that is the only check there is (the drill's later stages need a
cluster), which is why the guard exists; here it is exercised on the failure paths and against the mutant.

This drives the REAL payment-upgrade.sh with fake helm / kubectl / curl / sleep (no cluster):
  - fake helm: the 3rd `helm upgrade` is the slow background one (5 seconds, then it sets the release
    state to FLOW); every other `helm upgrade` is a restore/install (sets RESTORED). `helm template` is real.
    In the "install" scenario the first `helm upgrade` (the previous chart) fails.
  - fake curl: the first UpsertProduct after the background upgrade started answers 500, so order D1 fails.
  - fake kubectl: statements for the "app" database go to the real Postgres; the rest are canned.
Expected: the script exits non-zero (its exit code is kept), the release state ends RESTORED, the background helm
finished BEFORE the restore ran, the migrate Job's log is printed, and no process of the script's session is left.

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

REPO = pathlib.Path(__file__).resolve().parents[3]
# the values the real helm-kind passes: the chart must render with them (helm template is real)
values_file = REPO / "deployments/helm/kind/values.yaml"
flow_file = REPO / "deployments/helm/kind/payment-flow-values.yaml"
script_file = REPO / "deployments/helm/kind/payment-upgrade.sh"
guard_file = REPO / "deployments/helm/kind/payment-upgrade-guard.sh"  # what `task helm-kind` runs
helm_real = shutil.which("helm")
assert helm_real, "helm is needed (helm template is real)"
rt = shutil.which("docker") or shutil.which("podman")
assert rt, "docker or podman is needed (a real Postgres is the point of the isolation check)"
SENTINEL = "keep-me-" + uuid.uuid4().hex[:8]


def psql(container, db, sql):
    return subprocess.run([rt, "exec", container, "psql", "-U", "app", "-d", db, "-tAq", "-c", sql],
                          capture_output=True, text=True)


def start_postgres():
    name = "upgrade-failure-test-" + uuid.uuid4().hex[:8]
    subprocess.run([rt, "run", "-d", "--name", name, "-e", "POSTGRES_USER=app", "-e", "POSTGRES_PASSWORD=app",
                    "-e", "POSTGRES_DB=app", "postgres:16-alpine"], check=True, capture_output=True)
    ok = 0
    for _ in range(90):  # the image starts a throwaway server for its init, stops it, then starts the real one
        r = psql(name, "app", "select 1")
        ok = ok + 1 if r.returncode == 0 and r.stdout.strip() == "1" else 0
        if ok >= 3:
            return name
        time.sleep(1)
    subprocess.run([rt, "rm", "-f", name], capture_output=True)
    raise SystemExit("upgrade-failure-test: the Postgres container never became ready")


def run_scenario(script, mode, pg):
    """Run `script` (a copy of payment-upgrade.sh or a mutant) in `mode` ("install": the previous chart does not
    install; "upgrade": an order cannot be placed during the upgrade) and return the list of problems found."""
    root = pathlib.Path(tempfile.mkdtemp(prefix="upgrade-failure-test-"))
    bindir = root / "bin"
    bindir.mkdir()
    events = root / "events"
    # The release's own database holds something that is not this script's. Planted fresh for every run.
    for sql in ("DROP SCHEMA IF EXISTS unrelated CASCADE", "CREATE SCHEMA unrelated",
                "CREATE TABLE unrelated.keep (v text)", f"INSERT INTO unrelated.keep VALUES ('{SENTINEL}')"):
        assert psql(pg, "app", sql).returncode == 0, sql

    (bindir / "helm").write_text(f'''#!/bin/bash
if [ "$1" = template ]; then exec {helm_real} "$@"; fi
printf 'helm upgrade %s\\n' "$*" >> "$EVENTS"
n=$(grep -c '^helm upgrade' "$EVENTS")
if [ "$MODE" = install ] && [ "$n" = 1 ]; then exit 1; fi  # the previous chart does not install
if [ "$MODE" = upgrade ] && [ "$n" = 3 ]; then
  : > "$UPGRADE_STARTED"
  /bin/sleep 5
  echo HELM3_DONE >> "$EVENTS"
  echo FLOW > "$RELEASE_STATE"
else
  echo RESTORED > "$RELEASE_STATE"
fi
''')
    (bindir / "kubectl").write_text(f'''#!/bin/bash
printf 'kubectl %s\\n' "$*" >> "$EVENTS"
case "$*" in
  *"get deploy"*) echo 'ghcr.io/blackhorseya/x-monorepo/payment-rpc:0.63.3';;
  *" exec deploy/postgres "*)
    q=${{*: -1}}
    db=app
    prev=
    for a in "$@"; do [ "$prev" = -d ] && db=$a; prev=$a; done
    if [ "$db" = app ]; then
      # a statement for the release's own database: logged, and run for real
      printf '%s\\n' "$q" >> "$APPDB_LOG"
      {rt} exec "$PGC" psql -U app -d app -tAq -c "$q"
    else
      case "$q" in
        *"consumer_group, offset_acked"*)
          t=$(sed -n "s/.*SELECT '\\([^']*\\)'.*/\\1/p" <<<"$q")
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
    (root / "appdb.log").write_text("")

    env = os.environ.copy()
    env.update(PATH=f"{bindir}:{env['PATH']}", EVENTS=str(events), UPGRADE_STARTED=str(root / "upgrade_started"),
               ORDERS=str(root / "orders"), RELEASE_STATE=str(root / "release_state"), MODE=mode, PGC=pg,
               APPDB_LOG=str(root / "appdb.log"))
    env["UPGRADE_SCRIPT"] = str(script)  # the guard runs this copy of the drill
    cmd = ["bash", str(guard_file), "ns", "rel", "deployments/helm/x-monorepo", str(values_file), str(flow_file)]
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
        upgrades = [i for i, x in enumerate(lines) if x.startswith("helm upgrade")]
        done = [i for i, x in enumerate(lines) if x == "HELM3_DONE"]
        state = (root / "release_state").read_text().strip() if (root / "release_state").exists() else "none"
        if rc in (0, None):
            problems.append(f"exit code {rc}: a failed run must exit non-zero (the exit code is kept through the trap)")
        if state != "RESTORED":
            problems.append(f"the release ended in state {state}, want RESTORED (the restore must be the last change)")
        if mode == "upgrade":
            if not (root / "upgrade_started").exists():
                problems.append("the premise failed: the background upgrade never started, so nothing was tested")
            if len(upgrades) < 4:
                problems.append(f"{len(upgrades)} helm upgrades ran, want the old chart (x2), the background one and the restore (4)")
            elif not done:
                problems.append("the background helm upgrade never finished before the script exited")
            elif done[0] > upgrades[-1]:
                problems.append("the restore ran BEFORE the background helm upgrade finished: the upgrade overwrote it afterwards")
        else:
            if len(upgrades) != 2:
                problems.append(f"{len(upgrades)} helm upgrades ran, want the failed previous chart and the restore (2)")
        # a failure must say why the migrate hook failed: the Job's log, by its job-name label
        if not any(x.startswith("kubectl") and " logs " in x and "job-name=rel-migrate" in x for x in lines):
            problems.append("the failure did not print the logs of the rel-migrate Job")

        # ---- the release's own database is not touched (#427, #429) ----
        # the guard's own statements (its sentinel schema, its leftover-database query) are not the drill's
        stmts = [x for x in (root / "appdb.log").read_text().splitlines()
                 if x.strip() and "upgrade_guard_sentinel" not in x and "FROM pg_database" not in x]
        outtxt = (root / "out").read_text(errors="replace")
        if "guard: sentinel intact, no per-run database left" not in outtxt:
            problems.append("the guard (what task helm-kind runs) did not report the sentinel intact and no database left behind")
        create = [x for x in stmts if re.fullmatch(r'CREATE DATABASE "upgrade_[0-9]+_[0-9]+"', x)]
        drop = [x for x in stmts if re.fullmatch(r'DROP DATABASE IF EXISTS "upgrade_[0-9]+_[0-9]+" WITH \(FORCE\)', x)]
        other = [x for x in stmts if x not in create and x not in drop]
        if other:
            problems.append(f"statements sent to the release's own database besides its CREATE/DROP DATABASE: {other}")
        if len(create) != 1 or len(drop) != 1:
            problems.append(f"want exactly one CREATE DATABASE and one DROP DATABASE of the per-run database, got {create} / {drop}")
        elif re.search(r'"(.*)"', create[0]).group(1) != re.search(r'"(.*)"', drop[0]).group(1):
            problems.append("the database that was created is not the one that was dropped")
        first_up = lines.index(next(x for x in lines if x.startswith("helm upgrade"))) if upgrades else None
        created_at = next((i for i, x in enumerate(lines) if " exec deploy/postgres " in x and "CREATE DATABASE" in x), None)
        if created_at is None or (first_up is not None and created_at > first_up):
            problems.append("the per-run database was not created before the previous chart was installed")
        got = psql(pg, "app", "SELECT v FROM unrelated.keep")
        if got.returncode != 0 or got.stdout.strip() != SENTINEL:
            problems.append(f"the unrelated sentinel in the release's database is gone or changed: rc={got.returncode} {got.stdout.strip()!r} {got.stderr.strip()[:120]}")
        left = psql(pg, "app", "SELECT count(*) FROM pg_database WHERE datname LIKE 'upgrade\\_%'").stdout.strip()
        if left != "0":
            problems.append(f"{left} per-run database(s) left behind")
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


# The version that was rejected in review (#429): before the previous chart, drop every schema of the release's database.
BAD = ('for s in $(adb "SELECT nspname FROM pg_namespace WHERE nspname !~ \'^pg_\' AND nspname NOT IN (\'information_schema\', \'public\')"); '
       'do adb "DROP SCHEMA \\"$s\\" CASCADE" >/dev/null; done\n')


def main():
    pg = start_postgres()
    failed = False
    try:
        mutant_dir = pathlib.Path(tempfile.mkdtemp(prefix="upgrade-failure-mutant-"))
        mutant = mutant_dir / "payment-upgrade-mutant.sh"
        src = script_file.read_text()
        marker = "old_args+=("
        assert marker in src, "the script changed shape: the mutant cannot be built"
        mutant.write_text(src.replace(marker, BAD + marker, 1))
        for mode in ("install", "upgrade"):
            problems = run_scenario(script_file, mode, pg)
            if problems:
                failed = True
                print(f"upgrade-failure-test: FAIL ({mode})", file=sys.stderr)
                for x in problems:
                    print("  - " + x, file=sys.stderr)
            # fault injection: with the drop-everything step back, the same checks must go red
            caught = run_scenario(mutant, mode, pg)
            if not any("LOST" in x or "sentinel" in x or "statements sent to the release's own database" in x for x in caught):
                failed = True
                print(f"upgrade-failure-test: FAIL ({mode}): the drop-every-schema mutant was NOT caught: {caught}", file=sys.stderr)
        shutil.rmtree(mutant_dir, ignore_errors=True)
    finally:
        subprocess.run([rt, "rm", "-f", pg], capture_output=True)
    if failed:
        sys.exit(1)
    print("upgrade-failure-test: ok (exit code kept, release RESTORED, background helm joined before the restore, migrate log printed, "
          "the release's own database untouched, the drop-every-schema mutant caught, no child left)")


main()
