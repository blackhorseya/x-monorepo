#!/usr/bin/env python3
"""check-chart-bump.py / .sh: a change to what the chart renders needs a higher chart version (#423).

Chart.yaml says "bump on any template / values change", and a release whose templates changed under the
same version cannot be told apart from the one before it (#422: 0.3.57 was both the consumer that runs
payment-rpc and the one that runs payment-worker). The gate compares two chart directories: when anything
in templates/ or values*.yaml differs, Chart.yaml's version must be strictly greater (numerically: 0.3.10 >
0.3.9). Changes elsewhere (ci/, ci-negative/, a README, appVersion alone: the release bot) are not the chart.

Every case here drives the real tool on throwaway chart directories; the git wrapper is driven with a fake
`git` that serves the base from a fixture, and a git that fails must fail the gate, not pass it.

  deployments/helm/check-chart-bump-test.py     # task helm-check runs it
"""
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
TOOL = HERE / "check-chart-bump.py"
WRAPPER = HERE / "check-chart-bump.sh"
tmp = pathlib.Path(tempfile.mkdtemp(prefix="chart-bump-test-"))
failures = []


def chart(name, version="0.3.5", app="0.63.3", template="kind: Deployment\n", values="a: 1\n", extra=None):
    d = tmp / name
    (d / "templates").mkdir(parents=True)
    (d / "Chart.yaml").write_text(f'apiVersion: v2\nname: x\nversion: {version}\nappVersion: "{app}"\n')
    (d / "templates" / "deploy.yaml").write_text(template)
    (d / "values.yaml").write_text(values)
    for rel, text in (extra or {}).items():
        (d / rel).parent.mkdir(parents=True, exist_ok=True)
        (d / rel).write_text(text)
    return d


def run_tool(base, head):
    p = subprocess.run([sys.executable, str(TOOL), str(base), str(head)], capture_output=True, text=True)
    return p.returncode, (p.stdout + p.stderr).strip()


def expect(name, got, want_rc, must_say=None):
    rc, out = got
    ok = (rc == 0) if want_rc == 0 else (rc != 0 if want_rc == "nonzero" else rc == want_rc)
    if ok and must_say and must_say not in out:
        ok = False
    if not ok:
        failures.append(f"{name}: exit {rc} (want {want_rc}){' saying ' + repr(must_say) if must_say else ''}: {out[:200]}")


base = chart("base")
# nothing differs
expect("identical", run_tool(base, chart("h1")), 0)
# the release bot: appVersion and version move, no template or values
expect("appVersion and version only", run_tool(base, chart("h2", version="0.3.6", app="0.63.4")), 0)
# files that are not the chart
expect("ci profile and a README only", run_tool(base, chart("h3", extra={"ci/p.yaml": "x: 1\n", "ci-negative/n.yaml": "x: 2\n", "README.md": "hi\n"})), 0)
# a change to what renders, same version: red, and it says why
expect("template changed, same version", run_tool(base, chart("h4", template="kind: Job\n")), 1, "version")
expect("values changed, same version", run_tool(base, chart("h5", values="a: 2\n")), 1, "version")
expect("another values file changed, same version", run_tool(chart("b6", extra={"values-extra.yaml": "k: 1\n"}), chart("h6", extra={"values-extra.yaml": "k: 2\n"})), 1)
expect("template added, same version", run_tool(base, chart("h7", extra={"templates/new.yaml": "kind: X\n"})), 1)
expect("template removed, same version", run_tool(chart("b8", extra={"templates/old.yaml": "kind: X\n"}), chart("h8")), 1)
expect("template renamed, same version", run_tool(chart("b9", extra={"templates/old.yaml": "kind: X\n"}), chart("h9", extra={"templates/renamed.yaml": "kind: X\n"})), 1)
# the same change with a higher version: green
expect("template changed, patch bumped", run_tool(base, chart("h10", version="0.3.6", template="kind: Job\n")), 0)
expect("values changed, minor bumped", run_tool(base, chart("h11", version="0.4.0", values="a: 2\n")), 0)
# a version that is not higher is not a bump
expect("template changed, version lowered", run_tool(base, chart("h12", version="0.3.4", template="kind: Job\n")), 1)
# numeric, not lexical: 0.3.10 is higher than 0.3.9
expect("0.3.9 -> 0.3.10 with a template change", run_tool(chart("b13", version="0.3.9"), chart("h13", version="0.3.10", template="kind: Job\n")), 0)
expect("0.3.10 -> 0.3.9 with a template change", run_tool(chart("b14", version="0.3.10"), chart("h14", version="0.3.9", template="kind: Job\n")), 1)
# a version it cannot read is an error, never a pass
expect("unreadable version", run_tool(base, chart("h15", version="banana", template="kind: Job\n")), 2)
expect("missing Chart.yaml", run_tool(base, tmp / "nowhere"), "nonzero")

# the git wrapper: `check-chart-bump.sh <base-ref> [head-chart-dir]`, base from `git archive <ref> deployments/helm/x-monorepo`
fake = tmp / "fakebin"
fake.mkdir()
(fake / "git").write_text('''#!/bin/bash
case "$1" in
  archive) [ -n "$FAKE_BASE" ] || exit 1
           tar -c -C "$FAKE_BASE" deployments ;;
  fetch) exit "${FAKE_FETCH_RC:-0}" ;;
  *) exit 1 ;;
esac
''')
(fake / "git").chmod(0o755)


def base_tree(name, **kw):
    root = tmp / name
    chart_dir = root / "deployments/helm/x-monorepo"
    src = chart(name + "-src", **kw)
    chart_dir.parent.mkdir(parents=True)
    shutil.copytree(src, chart_dir)
    return root


def run_wrapper(fake_base, head, fetch_rc="0"):
    env = os.environ.copy()
    env.update(PATH=f"{fake}:{env['PATH']}", FAKE_BASE=str(fake_base) if fake_base else "", FAKE_FETCH_RC=fetch_rc)
    p = subprocess.run(["bash", str(WRAPPER), "origin/main", str(head)], capture_output=True, text=True, env=env, cwd=HERE.parent.parent)
    return p.returncode, (p.stdout + p.stderr).strip()


head_same = chart("hw1", template="kind: Job\n")
expect("wrapper: base has an older template, same version", run_wrapper(base_tree("bw1", version="0.3.5"), head_same), 1, "version")
expect("wrapper: base has an older template, lower version", run_wrapper(base_tree("bw2", version="0.3.4"), head_same), 0)
expect("wrapper: nothing changed", run_wrapper(base_tree("bw3", template="kind: Job\n"), head_same), 0)
expect("wrapper: git cannot serve the base (fail closed)", run_wrapper(None, head_same), "nonzero")
expect("wrapper: the fetch of the base fails (fail closed)", run_wrapper(base_tree("bw4", version="0.3.4"), head_same, fetch_rc="1"), "nonzero")

shutil.rmtree(tmp, ignore_errors=True)
if failures:
    print("check-chart-bump-test: FAIL", file=sys.stderr)
    for f in failures:
        print("  - " + f, file=sys.stderr)
    sys.exit(1)
print("check-chart-bump-test: ok")
