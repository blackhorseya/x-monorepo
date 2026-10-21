#!/usr/bin/env python3
"""Assertions on rendered manifests that lint / schema validation cannot make.

Run by `task helm-check` after helm lint and kubeconform, per ci/ profile:

    helm template t deployments/helm/x-monorepo -f <profile> | deployments/helm/check.py <profile>

The chart derives Kubernetes numbers from the apps' own shutdown behaviour; this is
the only logic in it, so it is the only thing tested here.
"""

import sys

import yaml


def main(profile: str) -> None:
    values = load_values(profile)
    docs = {}
    for d in yaml.safe_load_all(sys.stdin):
        if d:
            docs[(d["kind"], d["metadata"]["name"])] = d

    # The config file the pods read, parsed as the app parses it: YAML 1.1 turns an unquoted
    # off / on / yes / no into a boolean, and config.Load rejects `auth.mode: false`.
    cfg = yaml.safe_load(one(docs, "ConfigMap", "")["data"]["config.yaml"])
    check(cfg["auth"]["mode"] in ("off", "optional", "required"), f'auth.mode rendered as {cfg["auth"]["mode"]!r}')
    for path, v in walk(cfg):
        check(not isinstance(v, bool) or path[-1] in ("docs", "insecure"), f"{'.'.join(path)} rendered as boolean {v!r}")

    rpc = one(docs, "Deployment", "-rpc")
    worker = one(docs, "Deployment", "-worker")
    spec = rpc["spec"]["template"]["spec"]
    c = spec["containers"][0]
    env = {e["name"]: e.get("value") for e in c["env"]}
    sd = values["rpc"]["shutdown"]

    # rpc: the pod outlives delay + drain; readiness notices the 503 within delay.
    expect(spec["terminationGracePeriodSeconds"],
           sd["delaySeconds"] + sd["timeoutSeconds"] + sd["graceMarginSeconds"],
           "rpc terminationGracePeriodSeconds = delay + timeout + margin")
    expect(env["APP_HTTP_SHUTDOWN_DELAY"], f'{sd["delaySeconds"]}s', "APP_HTTP_SHUTDOWN_DELAY")
    expect(env["APP_HTTP_SHUTDOWN_TIMEOUT"], f'{sd["timeoutSeconds"]}s', "APP_HTTP_SHUTDOWN_TIMEOUT")
    check(c["readinessProbe"]["periodSeconds"] <= sd["delaySeconds"], "readiness period <= shutdown delay")
    expect(c["readinessProbe"]["httpGet"]["path"], "/api/readyz", "readiness path")
    expect(c["livenessProbe"]["httpGet"]["path"], "/api/healthz", "liveness path")
    expect(c["ports"][0]["containerPort"], int(values["config"]["http"]["addr"].rsplit(":", 1)[1]), "containerPort = http.addr")
    check(("replicas" in rpc["spec"]) != values["rpc"]["autoscaling"]["enabled"], "replicas omitted iff autoscaling")

    # worker: no HTTP server, so no probes; grace > 8s drain + 10s router close.
    wc = worker["spec"]["template"]["spec"]["containers"][0]
    check("livenessProbe" not in wc and "readinessProbe" not in wc, "worker has no probes")
    check(worker["spec"]["template"]["spec"]["terminationGracePeriodSeconds"] > 18, "worker grace > 18s")

    wa = values["worker"]["autoscaling"]
    check(("replicas" in worker["spec"]) != wa["enabled"], "worker replicas omitted iff autoscaling")
    hpa, so = find(docs, "HorizontalPodAutoscaler", "-worker"), find(docs, "ScaledObject", "-worker")
    check((hpa is not None) == (wa["enabled"] and wa["metric"] == "cpu"), "worker HPA iff autoscaling cpu")
    check((so is not None) == (wa["enabled"] and wa["metric"] == "lag"), "worker ScaledObject iff autoscaling lag")
    if so:
        trig = so["spec"]["triggers"][0]
        group = values["config"]["worker"]["consumer_group"]
        check(trig["type"] == "prometheus" and "max(" in trig["metadata"]["query"] and f'consumer_group=~"{group}\\\\..*"' in trig["metadata"]["query"],
              f'lag query takes max over the {group}.* groups: {trig["metadata"]["query"]}')
        expect(trig["metadata"]["threshold"], str(wa["lag"]["threshold"]), "lag threshold")
        expect(so["spec"]["scaleTargetRef"]["name"], worker["metadata"]["name"], "ScaledObject targets the worker")

    # migrate: a pre-install/pre-upgrade hook with a Secret it can actually read at that time.
    has_db = bool(values["secrets"].get("postgresUrl") or values["secrets"].get("existingSecret"))
    job = find(docs, "Job", "-migrate")
    check((job is not None) == (has_db and values["migrate"]["enabled"]), "migrate Job iff a Postgres URL is known")
    if job:
        expect(job["metadata"]["annotations"]["helm.sh/hook"], "pre-install,pre-upgrade", "migrate hook")
        ref = job["spec"]["template"]["spec"]["containers"][0]["envFrom"][0]["secretRef"]["name"]
        src = docs.get(("Secret", ref))
        if values["secrets"].get("existingSecret"):
            expect(ref, values["secrets"]["existingSecret"], "migrate reads existingSecret")
        else:
            check(src is not None and "helm.sh/hook" in src["metadata"].get("annotations", {}), "migrate Secret is hook-scoped")
            check(int(src["metadata"]["annotations"]["helm.sh/hook-weight"]) < int(job["metadata"]["annotations"]["helm.sh/hook-weight"]),
                  "migrate Secret is created before the Job")

    # prune: the CronJob runs the worker image with the retention from values.
    cron = find(docs, "CronJob", "-prune")
    check((cron is not None) == values["prune"]["enabled"], "prune CronJob iff enabled")
    if cron:
        pc = cron["spec"]["jobTemplate"]["spec"]["template"]["spec"]["containers"][0]
        check("/worker:" in pc["image"], "prune runs the worker image")
        check(f'--older-than={values["prune"]["olderThan"]}' in pc["args"], "prune --older-than")
        for g in values["prune"]["forgetGroups"]:
            check(f"--forget-group={g}" in pc["args"], f"prune --forget-group={g}")
        expect(cron["spec"]["concurrencyPolicy"], "Forbid", "prune never overlaps itself")

    # Every app pod reads the same config file and, when there is one, the same Secret.
    for d in (rpc, worker):
        cc = d["spec"]["template"]["spec"]["containers"][0]
        check(any(e["name"] == "APP_CONFIG" for e in cc["env"]), f'{d["metadata"]["name"]}: APP_CONFIG')
        check(("envFrom" in cc) == has_db or bool(values["secrets"].get("existingSecret")), f'{d["metadata"]["name"]}: envFrom iff secrets')

    print(f"helm check {profile}: ok")


def load_values(profile: str) -> dict:
    """values.yaml with the profile merged on top (one level deep is enough here)."""
    import os
    chart = os.path.join(os.path.dirname(os.path.abspath(__file__)), "x-monorepo")
    with open(os.path.join(chart, "values.yaml")) as f:
        base = yaml.safe_load(f)
    with open(profile) as f:
        over = yaml.safe_load(f) or {}
    return merge(base, over)


def merge(a, b):
    if isinstance(a, dict) and isinstance(b, dict):
        out = dict(a)
        for k, v in b.items():
            out[k] = merge(a.get(k), v) if k in a else v
        return out
    return b


def walk(node, path=()):
    if isinstance(node, dict):
        for k, v in node.items():
            yield from walk(v, path + (str(k),))
    else:
        yield path, node


def one(docs, kind, suffix):
    d = find(docs, kind, suffix)
    if d is None:
        fail(f"no {kind} named *{suffix}")
    return d


def find(docs, kind, suffix):
    for (k, name), d in docs.items():
        if k == kind and name.endswith(suffix):
            return d
    return None


def expect(got, want, what):
    check(got == want, f"{what}: got {got!r}, want {want!r}")


def check(ok, what):
    if not ok:
        fail(what)


def fail(msg):
    print(f"helm check FAILED: {msg}", file=sys.stderr)
    sys.exit(1)


if __name__ == "__main__":
    main(sys.argv[1])
