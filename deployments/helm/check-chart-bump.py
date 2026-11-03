#!/usr/bin/env python3
"""Fail when the chart's rendered inputs changed and Chart.yaml's version did not go up (#423).

  check-chart-bump.py <base-chart-dir> <head-chart-dir>

"The chart changed" = anything under templates/ or any top-level values*.yaml differs (added, removed,
renamed and edited files all count). ci/, ci-negative/, READMEs and Chart.yaml's appVersion are not the chart:
the release bot moves appVersion and the patch version together and touches nothing else.
Exit 0: nothing rendered changed, or the version is strictly higher (compared as numbers: 0.3.10 > 0.3.9).
Exit 1: the chart changed under the same or a lower version. Exit 2: a version could not be read.
Chart.yaml says "bump on any template / values change"; this makes it a gate, because two different charts
under one version cannot be told apart when something has to be rolled back.
"""
import os
import re
import sys


def inputs(chart):
    """relative path -> bytes, for templates/** and values*.yaml."""
    found = {}
    for root, _, files in os.walk(os.path.join(chart, "templates")):
        for f in files:
            p = os.path.join(root, f)
            found[os.path.relpath(p, chart)] = open(p, "rb").read()
    if os.path.isdir(chart):
        for f in os.listdir(chart):
            if re.fullmatch(r"values.*\.yaml", f):
                found[f] = open(os.path.join(chart, f), "rb").read()
    return found


def version(chart):
    try:
        text = open(os.path.join(chart, "Chart.yaml")).read()
    except OSError as e:
        _die(f"cannot read {chart}/Chart.yaml: {e.strerror}")
    m = re.search(r'^version:\s*["\']?([^\s"\']+)', text, re.M)
    if not m or not re.fullmatch(r"\d+\.\d+\.\d+", m.group(1)):
        _die(f"{chart}/Chart.yaml has no version x.y.z (found {m.group(1) if m else 'none'!r})")
    return tuple(int(n) for n in m.group(1).split(".")), m.group(1)


def _die(msg):
    print(f"check-chart-bump: {msg}", file=sys.stderr)
    sys.exit(2)


def main(base, head):
    bv, bs = version(base)
    hv, hs = version(head)
    b, h = inputs(base), inputs(head)
    changed = sorted(p for p in set(b) | set(h) if b.get(p) != h.get(p))
    if not changed:
        print(f"check-chart-bump: the chart's templates and values are unchanged (version {bs} -> {hs})")
        return 0
    if hv > bv:
        print(f"check-chart-bump: ok, {len(changed)} chart file(s) changed and the version went {bs} -> {hs}")
        return 0
    shown = ", ".join(changed[:8]) + (f" (+{len(changed) - 8} more)" if len(changed) > 8 else "")
    print(f"check-chart-bump: the chart changed ({shown}) but its version is {hs}, not higher than the base's {bs}: "
          f"bump `version` in deployments/helm/x-monorepo/Chart.yaml (appVersion stays), or a release of this chart "
          f"cannot be told from the one before it", file=sys.stderr)
    return 1


if __name__ == "__main__":
    if len(sys.argv) != 3:
        _die("usage: check-chart-bump.py <base-chart-dir> <head-chart-dir>")
    sys.exit(main(sys.argv[1], sys.argv[2]))
