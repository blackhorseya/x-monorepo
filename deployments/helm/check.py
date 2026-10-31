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
        check(not isinstance(v, bool) or path[-1] in ("docs", "insecure", "enabled"), f"{'.'.join(path)} rendered as boolean {v!r}")

    # auth.oidc: the list reaches the app as written (a mangled issuer or an off-by-default
    # `enabled` would turn the verifier on or off silently); lists are not walked above.
    oidc = cfg["auth"]["oidc"]
    expect(oidc["enabled"], values["config"]["auth"]["oidc"]["enabled"], "auth.oidc.enabled")
    expect(oidc["issuers"], values["config"]["auth"]["oidc"]["issuers"], "auth.oidc.issuers")
    for n, issuer in enumerate(oidc["issuers"]):
        check(issuer.get("issuer") and issuer.get("audience"), f"auth.oidc.issuers[{n}] needs issuer and audience")
        check(isinstance(issuer.get("allow_insecure_issuer", False), bool), f"auth.oidc.issuers[{n}].allow_insecure_issuer rendered as {issuer.get('allow_insecure_issuer')!r}")

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
    behavior = wa.get("behavior") or None
    if hpa:
        expect(hpa["spec"].get("behavior"), behavior, "worker HPA behavior passthrough")
    if so:
        expect(so["spec"].get("advanced", {}).get("horizontalPodAutoscalerConfig", {}).get("behavior"), behavior, "ScaledObject HPA behavior passthrough")
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

    # expire: the reservation TTL CronJob, same shape as prune.
    cron = find(docs, "CronJob", "-expire")
    check((cron is not None) == values["expire"]["enabled"], "expire CronJob iff enabled")
    if cron:
        ec = cron["spec"]["jobTemplate"]["spec"]["template"]["spec"]["containers"][0]
        check("/worker:" in ec["image"], "expire runs the worker image")
        check(ec["args"][0] == "expire", "expire runs `worker expire`")
        check(f'--older-than={values["expire"]["olderThan"]}' in ec["args"], "expire --older-than")
        check(("--dry-run" in ec["args"]) == bool(values["expire"]["dryRun"]), "expire --dry-run iff dryRun")
        expect(cron["spec"]["concurrencyPolicy"], "Forbid", "expire never overlaps itself")

    # console: nginx with the one env var, no app config, no Secret, /tmp writable, probes on itself.
    con = find(docs, "Deployment", "-console")
    cv = values["console"]
    check((con is not None) == cv["enabled"], "console Deployment iff enabled")
    check((find(docs, "Service", "-console") is not None) == cv["enabled"], "console Service iff enabled")
    check((find(docs, "Ingress", "-console") is not None) == (cv["enabled"] and cv["ingress"]["enabled"]), "console Ingress iff enabled")
    if con:
        cs = con["spec"]["template"]["spec"]
        cc = cs["containers"][0]
        cenv = {e["name"]: e.get("value") for e in cc["env"]}
        expect(cenv.get("RPC_UPSTREAM"), f'http://{rpc["metadata"]["name"]}:{values["rpc"]["service"]["port"]}', "console RPC_UPSTREAM = this release's rpc Service")
        check(not any(k.startswith("APP_") for k in cenv) and "envFrom" not in cc, "console gets no APP_* env and no Secret")
        check(any(m["mountPath"] == "/tmp" for m in cc["volumeMounts"]) and any("emptyDir" in v for v in cs["volumes"]), "console has an emptyDir at /tmp")
        check(cc["securityContext"].get("readOnlyRootFilesystem") is True, "console rootfs read-only")
        expect(cc["readinessProbe"]["httpGet"]["path"], "/healthz", "console readiness path")
        expect(cc["livenessProbe"]["httpGet"]["path"], "/healthz", "console liveness path")
        expect(cc["ports"][0]["containerPort"], 8080, "console containerPort = nginx-unprivileged's 8080")

    # ServiceMonitor: scrapes the rpc Service's http port at /api/metrics.
    sm = find(docs, "ServiceMonitor", "-rpc")
    check((sm is not None) == values["rpc"]["serviceMonitor"]["enabled"], "ServiceMonitor iff enabled")
    if sm:
        svc = one(docs, "Service", "-rpc")
        ep = sm["spec"]["endpoints"][0]
        expect(ep["path"], "/api/metrics", "ServiceMonitor path")
        expect(ep["port"], svc["spec"]["ports"][0]["name"], "ServiceMonitor port = the rpc Service's port name")
        check(all(svc["metadata"]["labels"].get(k) == v for k, v in sm["spec"]["selector"]["matchLabels"].items()), "ServiceMonitor selects the rpc Service")
        for k, v in values["rpc"]["serviceMonitor"]["labels"].items():
            expect(sm["metadata"]["labels"].get(k), v, f"ServiceMonitor label {k}")

    # NetworkPolicy: ingress-only, one per Deployment; the console may always reach the rpc.
    npv = values["networkPolicy"]
    for comp in ("rpc", "worker", "console"):
        pol = find(docs, "NetworkPolicy", f"-{comp}")
        want = npv["enabled"] and (comp != "console" or cv["enabled"])
        check((pol is not None) == want, f"NetworkPolicy {comp} iff enabled")
        if pol:
            expect(pol["spec"]["policyTypes"], ["Ingress"], f"NetworkPolicy {comp} is ingress-only")
            expect(pol["spec"]["podSelector"]["matchLabels"].get("app.kubernetes.io/component"), comp, f"NetworkPolicy {comp} selects its pods")
    if npv["enabled"]:
        rp = one(docs, "NetworkPolicy", "-rpc")["spec"]["ingress"][0]
        expect(rp["ports"], [{"protocol": "TCP", "port": c["ports"][0]["containerPort"]}], "rpc NetworkPolicy port = http.addr")
        peers = rp.get("from", [])
        from_console = [p for p in peers if p.get("podSelector", {}).get("matchLabels", {}).get("app.kubernetes.io/component") == "console"]
        check(bool(from_console) == cv["enabled"], "rpc NetworkPolicy admits the console iff the console is deployed")
        check(len(peers) == len(from_console) + len(npv["rpc"]["ingressFrom"]), "rpc NetworkPolicy peers = console + ingressFrom")
        check(("from" in rp) == bool(peers), "rpc NetworkPolicy: no peers listed means any source")
        expect(one(docs, "NetworkPolicy", "-worker")["spec"]["ingress"], [], "worker NetworkPolicy denies all ingress")
        if cv["enabled"]:
            cp = one(docs, "NetworkPolicy", "-console")["spec"]["ingress"][0]
            expect(cp["ports"], [{"protocol": "TCP", "port": 8080}], "console NetworkPolicy port")
            expect(cp.get("from", []), npv["console"]["ingressFrom"], "console NetworkPolicy peers = ingressFrom")

    # catalog-rpc (slice 3 of #320): off by default (no image of that name exists at the chart's
    # appVersion until a release has built it); when on it runs on its OWN ConfigMap and Secret,
    # because it refuses to start on auth.jwt / auth.oidc.enabled / auth on without api_keys and
    # the release-wide config and Secret carry exactly those for rpc.
    cr = values.get("catalogRpc", {})
    cdep = find(docs, "Deployment", "-catalog-rpc")
    check((cdep is not None) == bool(cr.get("enabled")), "catalog-rpc Deployment iff catalogRpc.enabled")
    for kind in ("Service", "ConfigMap"):
        check((find(docs, kind, "-catalog-rpc") is not None) == bool(cr.get("enabled")), f"catalog-rpc {kind} iff enabled")
    if cdep and cr.get("enabled"):
        check_catalog_rpc(docs, values, cdep, cr)

    # Every app pod reads the same config file and, when there is one, the same Secret.
    for d in (rpc, worker):
        cc = d["spec"]["template"]["spec"]["containers"][0]
        check(any(e["name"] == "APP_CONFIG" for e in cc["env"]), f'{d["metadata"]["name"]}: APP_CONFIG')
        check(("envFrom" in cc) == has_db or bool(values["secrets"].get("existingSecret")), f'{d["metadata"]["name"]}: envFrom iff secrets')

    print(f"helm check {profile}: ok")


def check_catalog_rpc(docs, values, dep, cr):
    spec = dep["spec"]["template"]["spec"]
    c = spec["containers"][0]
    name = dep["metadata"]["name"]
    check(c["image"].split("/")[-1].startswith("catalog-rpc:"), f"catalog-rpc image is catalog-rpc: {c['image']}")
    sd = cr["shutdown"]
    expect(spec["terminationGracePeriodSeconds"], sd["delaySeconds"] + sd["timeoutSeconds"] + sd["graceMarginSeconds"], "catalog-rpc terminationGracePeriodSeconds = delay + timeout + margin")
    expect(c["readinessProbe"]["httpGet"]["path"], "/api/readyz", "catalog-rpc readiness path")
    expect(c["livenessProbe"]["httpGet"]["path"], "/api/healthz", "catalog-rpc liveness path")
    check(c["readinessProbe"]["periodSeconds"] <= sd["delaySeconds"], "catalog-rpc readiness period <= shutdown delay")
    expect(c["ports"][0]["containerPort"], int(values["config"]["http"]["addr"].rsplit(":", 1)[1]), "catalog-rpc containerPort = http.addr")

    # Its own config: auth required, the admin role, and NOTHING of rpc's credential sources.
    cm = one(docs, "ConfigMap", "-catalog-rpc")
    cfg = yaml.safe_load(cm["data"]["config.yaml"])
    expect(cfg["auth"].get("mode"), "required", "catalog-rpc auth.mode")
    expect(cfg["auth"].get("admin_role"), values["config"]["auth"]["admin_role"], "catalog-rpc auth.admin_role = the release's")
    check(set(cfg["auth"]) <= {"mode", "admin_role"}, f"catalog-rpc auth carries only mode and admin_role, got {sorted(cfg['auth'])}")
    check("jwt" not in cfg["auth"] and "oidc" not in cfg["auth"], "catalog-rpc config has no auth.jwt / auth.oidc")
    check("service_name" not in cfg.get("otel", {}), "catalog-rpc does not inherit otel.service_name (it would report as rpc)")
    for k in ("http", "postgres", "log", "otel"):
        expect(cfg.get(k), {kk: vv for kk, vv in values["config"][k].items() if kk != "service_name"}, f"catalog-rpc {k} = the release's")
    check("worker" not in cfg, "catalog-rpc config has no worker section")

    # Its own env: exactly the catalog-rpc Secret, never the release-wide one (which carries rpc's
    # JWT secret / user api keys).
    secret_ref = [e["secretRef"]["name"] for e in c.get("envFrom", [])]
    if cr_secret := cr_secret_name(values, docs):
        expect(secret_ref, [cr_secret], "catalog-rpc envFrom = its own Secret only")
    shared = find(docs, "Secret", "-env")
    check(shared is None or shared["metadata"]["name"] not in secret_ref, "catalog-rpc never reads the release-wide Secret")
    if values["secrets"].get("existingSecret"):
        check(values["secrets"]["existingSecret"] not in secret_ref, "catalog-rpc never reads secrets.existingSecret")
    env = {e["name"]: e.get("value") for e in c["env"]}
    check(not any(k.startswith("APP_AUTH_") for k in env), "catalog-rpc has no APP_AUTH_* env of its own making")
    expect(env["APP_CONFIG"], "/etc/x-monorepo/config.yaml", "catalog-rpc APP_CONFIG")
    vols = {v["name"]: v for v in spec["volumes"]}
    expect(vols["config"]["configMap"]["name"], cm["metadata"]["name"], "catalog-rpc mounts its own ConfigMap")

    # Its own Secret (unless the operator supplies one): Postgres URL and the single service key.
    if not values["secrets"]["catalogRpc"].get("existingSecret"):
        sec = one(docs, "Secret", "-catalog-rpc-env")
        data = sec["stringData"]
        expect(sorted(data), ["APP_AUTH_API_KEYS", "APP_POSTGRES_URL"], "catalog-rpc Secret keys")
        expect(data["APP_AUTH_API_KEYS"], f'rpc={values["secrets"]["catalogRpc"]["apiKey"]}:{values["config"]["auth"]["admin_role"]}', "catalog-rpc service key is rpc=<key>:<admin role>")
        expect(data["APP_POSTGRES_URL"], values["secrets"]["postgresUrl"], "catalog-rpc Postgres URL")
    else:
        check(find(docs, "Secret", "-catalog-rpc-env") is None, "no catalog-rpc Secret rendered when secrets.catalogRpc.existingSecret is set")

    svc = one(docs, "Service", "-catalog-rpc")
    expect(svc["spec"]["selector"]["app.kubernetes.io/component"], "catalog-rpc", "catalog-rpc Service selects catalog-rpc pods")
    expect(svc["spec"]["ports"][0]["port"], cr["service"]["port"], "catalog-rpc Service port")

    pdb = find(docs, "PodDisruptionBudget", "-catalog-rpc")
    check((pdb is not None) == (cr["podDisruptionBudget"]["enabled"] and cr["replicaCount"] > 1), "catalog-rpc PDB iff enabled and more than one replica")

    sm = find(docs, "ServiceMonitor", "-catalog-rpc")
    check((sm is not None) == cr["serviceMonitor"]["enabled"], "catalog-rpc ServiceMonitor iff enabled")
    if sm:
        ep = sm["spec"]["endpoints"][0]
        expect(ep["path"], "/api/metrics", "catalog-rpc ServiceMonitor path")
        expect(ep["port"], svc["spec"]["ports"][0]["name"], "catalog-rpc ServiceMonitor port = the Service's port name")
        check(all(svc["metadata"]["labels"].get(k) == v for k, v in sm["spec"]["selector"]["matchLabels"].items()), "catalog-rpc ServiceMonitor selects the catalog-rpc Service")

    # NetworkPolicy: ingress from the rpc pods of this release on the app port, nobody else.
    pol = find(docs, "NetworkPolicy", "-catalog-rpc")
    check((pol is not None) == values["networkPolicy"]["enabled"], "catalog-rpc NetworkPolicy iff networkPolicy.enabled")
    if pol:
        expect(pol["spec"]["policyTypes"], ["Ingress"], "catalog-rpc NetworkPolicy is ingress-only")
        expect(pol["spec"]["podSelector"]["matchLabels"].get("app.kubernetes.io/component"), "catalog-rpc", "catalog-rpc NetworkPolicy selects its pods")
        rules = pol["spec"]["ingress"]
        check(len(rules) == 1, "catalog-rpc NetworkPolicy has one ingress rule")
        expect(rules[0]["ports"], [{"protocol": "TCP", "port": c["ports"][0]["containerPort"]}], "catalog-rpc NetworkPolicy port = http.addr")
        peers = rules[0].get("from")
        check(peers is not None and len(peers) == 1, "catalog-rpc NetworkPolicy lists exactly one peer (empty would mean any source)")
        sel = peers[0]["podSelector"]["matchLabels"]
        expect(sel.get("app.kubernetes.io/component"), "rpc", "catalog-rpc NetworkPolicy admits the rpc pods")
        expect(sel.get("app.kubernetes.io/instance"), dep["metadata"]["labels"]["app.kubernetes.io/instance"], "...of this release only")


def cr_secret_name(values, docs):
    ex = values["secrets"]["catalogRpc"].get("existingSecret")
    if ex:
        return ex
    sec = find(docs, "Secret", "-catalog-rpc-env")
    return sec["metadata"]["name"] if sec else None


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
        if k != kind or not name.endswith(suffix):
            continue
        # "-rpc" is the rpc's, not the catalog-rpc's; the shared ConfigMap ("" suffix) and
        # Secret are the release's, not catalog-rpc's own.
        if suffix != "-catalog-rpc" and name.endswith("-catalog-rpc"):
            continue
        if suffix in ("", "-env") and "-catalog-rpc" in name:
            continue
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
