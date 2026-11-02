#!/usr/bin/env python3
"""The payment panels and alert rules exist, and say only metric names the code really exports.

Run by `task helm-check` (the chart's payment-rpc is observed through these; nothing else in the chart
renders them): the Grafana dashboard and the alert rules live in deployments/docker/grafana and are
provisioned from there, so a typo in a metric name makes a rule that silently never fires.

    deployments/helm/check-observability.py

Every metric name in a payment panel or rule is looked up in the Go source after the suffixes Prometheus
adds are taken off: `payment_*` as the quoted instrument name, `eventbus_*` in the Prometheus-names list that
platform/eventbus/metrics.go documents above its (dotted) instruments.
"""

import json
import os
import re
import subprocess
import sys

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
DASH = os.path.join(ROOT, "deployments/docker/grafana/dashboards/rpc.json")
ALERTS = os.path.join(ROOT, "deployments/docker/grafana/provisioning/alerting.yaml")
DASH_UID = "x-monorepo-rpc"

METRIC = re.compile(r"\b((?:payment|eventbus)_[a-z0-9_]+)")
SUFFIXES = ("_bucket", "_count", "_sum")


def main() -> None:
    dash = json.load(open(DASH))
    panels = {p["id"]: p for p in dash["panels"]}
    check(len(panels) == len(dash["panels"]), "dashboard panel ids are unique")

    rows = [p for p in dash["panels"] if p["type"] == "row" and p["title"].startswith("Payment")]
    check(len(rows) == 1, f"the dashboard has one Payment row, found {len(rows)}")
    row = rows[0]
    # the panels of the row: everything after it up to the next row
    order = dash["panels"]
    inside = []
    for p in order[order.index(row) + 1:]:
        if p["type"] == "row":
            break
        inside.append(p)
    exprs = " ".join(t["expr"] for p in inside for t in p.get("targets", []))
    for want in ("payment_reconcile_eligible", "payment_reconcile_oldest_unchecked_seconds", "payment_alerts_total"):
        check(want in exprs, f"the Payment row has a panel on {want}")
    check("eventbus_consumer_lag" in exprs and 'consumer_group=~"payment-rpc' in exprs,
          "the Payment row has the consumer lag of payment-rpc's own consumer group")
    for p in inside:
        check(p["gridPos"]["y"] > row["gridPos"]["y"], f"panel {p['id']} sits below its row")

    groups = {g["name"]: g for g in yaml.safe_load(open(ALERTS))["groups"]}
    check("payment" in groups, "alerting.yaml has a payment group")
    rules = groups["payment"]["rules"]
    uids = {r["uid"] for r in rules}
    for want in ("x-monorepo-payment-alerts", "x-monorepo-payment-reconcile-stale", "x-monorepo-payment-reconcile-absent"):
        check(want in uids, f"the payment group has the rule {want}")
    for r in rules:
        check(r["uid"].startswith("x-monorepo-payment-"), f"{r['uid']}: uid prefix")
        check(r["annotations"].get("__dashboardUid__") == DASH_UID, f"{r['uid']} links the dashboard")
        pid = int(r["annotations"]["__panelId__"])
        check(pid in panels and any(pid == p["id"] for p in inside), f"{r['uid']} links panel {pid}, which is not in the Payment row")
        expr = " ".join(d["model"].get("expr", "") for d in r["data"])
        check(METRIC.search(expr) is not None, f"{r['uid']} queries a payment metric")
        names(r["uid"], expr)
    for p in inside:
        for t in p.get("targets", []):
            names(f"panel {p['id']} {p['title']!r}", t["expr"])
    print("helm check observability: ok")


def names(where: str, expr: str) -> None:
    for m in METRIC.findall(expr):
        base = m
        for s in SUFFIXES:
            if base.endswith(s):
                base = base[: -len(s)]
        check(in_code(base) or in_code(m), f"{where}: metric {m!r} is not exported by any instrument in the code (a typo makes a rule that never fires)")


_cache: dict = {}


def in_code(name: str) -> bool:
    if name not in _cache:
        if name.startswith("eventbus_"):
            # the eventbus instruments are dotted (eventbus.consumer.lag) and Prometheus renames them; the
            # package lists the Prometheus names in the comment above its instruments
            _cache[name] = re.search(rf"\b{name}\b", open(os.path.join(ROOT, "pkgs/platform/eventbus/metrics.go")).read()) is not None
        else:
            out = subprocess.run(["grep", "-rIl", "--include=*.go", "--exclude=*_test.go", f'"{name}"', "pkgs", "apps"], cwd=ROOT, capture_output=True, text=True)
            _cache[name] = bool(out.stdout.strip())
    return _cache[name]


def check(ok, what):
    if not ok:
        print(f"helm check observability FAILED: {what}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
