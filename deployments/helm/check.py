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
        check(not isinstance(v, bool) or path[-1] in ("docs", "insecure", "enabled", "require_payment"), f"{'.'.join(path)} rendered as boolean {v!r}")

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

    # rpc asks catalog-rpc (#320 slice 4): where it is, which key it presents, and a best-effort wait before
    # it starts. rpc has no in-process catalog any more, so catalog-rpc is on in every profile.
    cr0 = values.get("catalogRpc", {})
    check(cr0.get("enabled"), "catalogRpc is on: rpc has no in-process catalog to fall back on")
    check_rpc_to_catalog(docs, values, rpc, c, spec)

    # payment-rpc (#367 slice 5c): off by default, and then the release is what it was before it existed.
    check_payment_rpc(docs, values, rpc, c, worker)

    # The release-wide ConfigMap (the worker, rpc and the CronJobs mount it) never carries a payment credential,
    # whatever the values say: the provider's credential belongs to payment-rpc's own Secret alone.
    for k in ("mock_webhook_secret", "api_key"):
        check(k not in (cfg.get("payment") or {}), f"the release-wide ConfigMap carries payment.{k}")

    # Every app pod reads the same config file and, when there is one, the same Secret.
    for d in (rpc, worker):
        cc = d["spec"]["template"]["spec"]["containers"][0]
        check(any(e["name"] == "APP_CONFIG" for e in cc["env"]), f'{d["metadata"]["name"]}: APP_CONFIG')
        check(("envFrom" in cc) == has_db or bool(values["secrets"].get("existingSecret")), f'{d["metadata"]["name"]}: envFrom iff secrets')

    print(f"helm check {profile}: ok")


def check_rpc_to_catalog(docs, values, rpc, c, spec):
    cr = values["catalogRpc"]
    renv = {e["name"]: e for e in c["env"]}
    svc = one(docs, "Service", "-catalog-rpc")
    check("APP_CATALOG_UPSTREAM" in renv and "APP_CATALOG_API_KEY" in renv, "rpc is told where catalog-rpc is and with which key (APP_CATALOG_UPSTREAM, APP_CATALOG_API_KEY)")
    expect(renv["APP_CATALOG_UPSTREAM"].get("value"), f'http://{svc["metadata"]["name"]}:{cr["service"]["port"]}', "rpc APP_CATALOG_UPSTREAM = the catalog-rpc Service of this release")

    # the service key: the SAME Secret and key catalog-rpc is configured from, never a copy typed twice
    own = cr_secret_name(values, docs)
    ref = renv["APP_CATALOG_API_KEY"]["valueFrom"]["secretKeyRef"]
    expect(ref.get("name"), own, "rpc APP_CATALOG_API_KEY comes from catalog-rpc's own Secret")
    expect(ref.get("key"), "APP_CATALOG_API_KEY", "rpc APP_CATALOG_API_KEY secretKeyRef key")
    expect(ref.get("optional"), False, "rpc APP_CATALOG_API_KEY optional: false (a Secret without it keeps the pod from starting)")
    if not values["secrets"]["catalogRpc"].get("existingSecret"):
        data = one(docs, "Secret", "-catalog-rpc-env")["stringData"]
        k = values["secrets"]["catalogRpc"]["apiKey"]
        expect(data["APP_CATALOG_API_KEY"], k, "rpc's key is the catalogRpc.apiKey value")
        expect(data["APP_AUTH_API_KEYS"], f'rpc={k}:{values["config"]["auth"]["admin_role"]}', "...and catalog-rpc accepts exactly that key, named rpc")
    # nothing else of catalog-rpc's Secret reaches rpc, and the shared Secret is not catalog-rpc's
    check(not any("secretRef" in e and e["secretRef"].get("name") == own for e in c.get("envFrom", [])), "rpc does not envFrom catalog-rpc's Secret (only the one key by secretKeyRef)")
    names = [e["name"] for e in c["env"]]
    check(len(names) == len(set(names)), f"rpc env has no duplicate names: {names}")

    # the best-effort wait: an initContainer on the rpc image that cannot hold a rollout longer than its budget
    w = values["rpc"].get("waitForCatalog", {})
    inits = spec.get("initContainers", [])
    check(bool(inits) == bool(w.get("enabled")), "rpc initContainer iff rpc.waitForCatalog.enabled")
    if w.get("enabled"):
        check(len(inits) == 1, "one initContainer")
        ic = inits[0]
        expect(ic["name"], "wait-for-catalog", "initContainer name")
        expect(ic["image"], c["image"], "the wait runs the rpc image (distroless: the binary does the waiting)")
        expect(ic.get("args"), [f'--wait-catalog={w["timeoutSeconds"]}s'], "initContainer args")
        ienv = {e["name"]: e.get("value") for e in ic.get("env", [])}
        expect(ienv.get("APP_CATALOG_UPSTREAM"), renv["APP_CATALOG_UPSTREAM"]["value"], "initContainer waits for the same upstream")
        check("APP_CATALOG_API_KEY" not in ienv and "envFrom" not in ic, "the wait presents no credential (the probe path is open)")
        check(ic["securityContext"].get("readOnlyRootFilesystem") is True, "initContainer rootfs read-only")
        deadline = rpc["spec"].get("progressDeadlineSeconds")
        check(deadline is not None and w["timeoutSeconds"] < deadline, f'wait {w["timeoutSeconds"]}s < progressDeadlineSeconds {deadline}')


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

    # Its own env: the two Secret keys it cannot run without come in by secretKeyRef (optional: false),
    # never envFrom: an external Secret missing APP_POSTGRES_URL must keep the pod from starting,
    # not let it serve from memory with readyz 200. The Secret is its own, never the release-wide one
    # (rpc's JWT secret and user api keys).
    # The guarantee that cannot be made at template time (an external Secret holding an empty URL, an
    # env var saying auth off) is made by the binary: --require-durable, as a container ARG, which no
    # env var or Secret can change. Nothing else is passed.
    expect(c.get("args"), ["--require-durable"], "catalog-rpc container args = --require-durable (refuses in-memory / non-required auth at start)")
    check("command" not in c, "catalog-rpc keeps the image ENTRYPOINT (no command override)")
    # A refusal must be readable after the container is gone: the last log lines are copied into the Pod
    # status (terminated.message), which does not depend on the node still holding the log file.
    expect(c.get("terminationMessagePolicy"), "FallbackToLogsOnError", "catalog-rpc terminationMessagePolicy (a refusal stays readable in the Pod status)")
    check("envFrom" not in c, "catalog-rpc has no envFrom (a missing key would be silently skipped)")
    own = cr_secret_name(values, docs)
    check(own is not None, "catalog-rpc has a Secret to read")
    env_list = c["env"]
    refs = {e["name"]: e["valueFrom"]["secretKeyRef"] for e in env_list if "valueFrom" in e}
    expect(sorted(refs), ["APP_AUTH_API_KEYS", "APP_POSTGRES_URL"], "catalog-rpc reads exactly APP_POSTGRES_URL and APP_AUTH_API_KEYS from a Secret")
    for k, ref in refs.items():
        expect(ref.get("name"), own, f"catalog-rpc {k} comes from its own Secret")
        expect(ref.get("key"), k, f"catalog-rpc {k} secretKeyRef key")
        expect(ref.get("optional"), False, f"catalog-rpc {k} secretKeyRef optional: false (a missing key keeps the pod from starting)")
    shared = find(docs, "Secret", "-env")
    check(shared is None or shared["metadata"]["name"] != own, "catalog-rpc never reads the release-wide Secret")
    if values["secrets"].get("existingSecret"):
        check(values["secrets"]["existingSecret"] != own, "catalog-rpc never reads secrets.existingSecret")
    names = [e["name"] for e in env_list]
    check(len(names) == len(set(names)), f"catalog-rpc env has no duplicate names: {names}")
    env = {e["name"]: e.get("value") for e in env_list}
    check(not any(k.startswith("APP_AUTH_") and k != "APP_AUTH_API_KEYS" for k in env), "catalog-rpc has no APP_AUTH_* env but its api key")
    expect(env["APP_CONFIG"], "/etc/x-monorepo/config.yaml", "catalog-rpc APP_CONFIG")
    # operator extraEnv comes first; what the chart sets (secret refs, config path, shutdown) comes after it
    for e in cr.get("extraEnv", []):
        check(names.index(e["name"]) < names.index("APP_CONFIG") and names.index(e["name"]) < names.index("APP_AUTH_API_KEYS"),
              f"catalog-rpc extraEnv {e['name']} is listed before the chart's own env")
    vols = {v["name"]: v for v in spec["volumes"]}
    expect(vols["config"]["configMap"]["name"], cm["metadata"]["name"], "catalog-rpc mounts its own ConfigMap")

    # Its own Secret (unless the operator supplies one): Postgres URL and the single service key.
    if not values["secrets"]["catalogRpc"].get("existingSecret"):
        sec = one(docs, "Secret", "-catalog-rpc-env")
        data = sec["stringData"]
        expect(sorted(data), ["APP_AUTH_API_KEYS", "APP_CATALOG_API_KEY", "APP_POSTGRES_URL"], "catalog-rpc Secret keys (catalog-rpc itself reads two; the third is rpc's copy of the same key)")
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
        extra = values["networkPolicy"].get("catalogRpc", {}).get("ingressFrom", [])
        check(peers is not None and len(peers) == 1 + len(extra), "catalog-rpc NetworkPolicy peers = rpc + networkPolicy.catalogRpc.ingressFrom (never empty: empty would mean any source)")
        sel = peers[0]["podSelector"]["matchLabels"]
        expect(sel.get("app.kubernetes.io/component"), "rpc", "catalog-rpc NetworkPolicy admits the rpc pods")
        expect(sel.get("app.kubernetes.io/instance"), dep["metadata"]["labels"]["app.kubernetes.io/instance"], "...of this release only")


PAYMENT_SERVE_SECRET_KEYS = ["APP_AUTH_API_KEYS", "APP_PAYMENT_MOCK_WEBHOOK_SECRET", "APP_POSTGRES_URL"]
PAYMENT_BACKGROUND_SECRET_KEYS = ["APP_PAYMENT_MOCK_WEBHOOK_SECRET", "APP_POSTGRES_URL"]
WEBHOOK_PATH = "/api/payment/webhook"


def check_payment_rpc(docs, values, rpc, rc, worker):
    pv = values.get("paymentRpc", {"enabled": False})
    on = bool(pv["enabled"])
    renv = {e["name"]: e for e in rc["env"]}

    # The worker takes the payment_required snapshot, so with ordering.require_payment on it is told where payment-rpc is
    # (payment.upstream, the address only: the service key is rpc's credential and a worker holding it could call
    # payment-rpc as rpc), and it refuses to start without it. With the switch off it is told nothing about payment.
    wenv = {e["name"]: e for e in worker["spec"]["template"]["spec"]["containers"][0]["env"]}
    pay_env = sorted(n for n in wenv if n.startswith("APP_PAYMENT_"))
    if (values["config"].get("ordering") or {}).get("require_payment"):
        expect(pay_env, ["APP_PAYMENT_UPSTREAM"], "with ordering.require_payment on, the worker is told the payment address and nothing else of payment")
        expect(wenv["APP_PAYMENT_UPSTREAM"].get("value"), renv["APP_PAYMENT_UPSTREAM"].get("value"), "the worker's payment.upstream is the one rpc has")
    else:
        expect(pay_env, [], "with ordering.require_payment off, the worker is told nothing about payment")

    # Off (the default): not one object, and not one setting in the other workloads. "Before it is on, the
    # release behaves as it did" is exactly this.
    objs = [("Deployment", "-payment-serve"), ("Deployment", "-payment-consume"), ("CronJob", "-payment-expire"),
            ("Service", "-payment-serve"), ("ConfigMap", "-payment-rpc"), ("Secret", "-payment-rpc-env"),
            ("Ingress", "-payment-webhook"), ("PodDisruptionBudget", "-payment-serve"), ("ServiceMonitor", "-payment-serve"),
            ("NetworkPolicy", "-payment-serve"), ("NetworkPolicy", "-payment-consume"), ("NetworkPolicy", "-payment-expire")]
    if not on:
        for kind, suffix in objs:
            check(find(docs, kind, suffix) is None, f"{kind} *{suffix} is rendered while paymentRpc is off")
        check(not any(n.startswith("APP_PAYMENT_") for n in renv), "rpc has APP_PAYMENT_* env while paymentRpc is off")
        for (kind, name) in docs:
            check("payment" not in name, f"{kind} {name}: something payment-shaped is rendered while paymentRpc is off")
        return

    # --- three workloads, one image ---
    serve = one(docs, "Deployment", "-payment-serve")
    consume = one(docs, "Deployment", "-payment-consume")
    cron = one(docs, "CronJob", "-payment-expire")
    svc = one(docs, "Service", "-payment-serve")
    cm = one(docs, "ConfigMap", "-payment-rpc")
    own_secret = payment_secret_name(values, docs)
    check(own_secret is not None, "payment-rpc has a Secret to read")
    ss = serve["spec"]["template"]["spec"]
    sc = ss["containers"][0]
    cs = consume["spec"]["template"]["spec"]
    cc = cs["containers"][0]
    xs = cron["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    xc = xs["containers"][0]
    # #410 slice 2: consume runs the payment-worker image (its own binary); serve and expire stay payment-rpc.
    for what, c_, app in (("serve", sc, "payment-rpc"), ("consume", cc, "payment-worker"), ("expire", xc, "payment-rpc")):
        check(c_["image"].split("/")[-1].startswith(app + ":"), f"payment {what} runs the {app} image: {c_['image']}")
        expect(c_["name"], app, f"payment {what} container name")
        check("command" not in c_, f"payment-rpc {what} keeps the image ENTRYPOINT")
        check("envFrom" not in c_, f"payment-rpc {what} has no envFrom (a missing key would be silently skipped)")
        check(c_["securityContext"].get("readOnlyRootFilesystem") is True, f"payment-rpc {what} rootfs read-only")
    # The binary refuses to start on a configuration that would serve payments from memory or without
    # required auth, judged on the FINAL effective settings; as a container arg no env var or Secret can change it.
    expect(sc.get("args"), ["serve", "--require-durable"], "payment-rpc serve args")
    expect(cc.get("args"), ["--require-durable"], "payment-worker args (the binary has no subcommand; the flag is an arg, no env var can change it)")
    expect(xc.get("args"), ["expire", "--require-durable"], "payment-rpc expire args")
    for what, c_ in (("serve", sc), ("consume", cc), ("expire", xc)):
        expect(c_.get("terminationMessagePolicy"), "FallbackToLogsOnError", f"payment-rpc {what} terminationMessagePolicy (a refusal stays readable in the Pod status)")

    # --- its own ConfigMap: auth required, no inherited credential source, the Postgres broker ---
    pcfg = yaml.safe_load(cm["data"]["config.yaml"])
    expect(pcfg["auth"].get("mode"), "required", "payment-rpc auth.mode")
    expect(pcfg["auth"].get("admin_role"), values["config"]["auth"]["admin_role"], "payment-rpc auth.admin_role = the release's")
    check(set(pcfg["auth"]) <= {"mode", "admin_role"}, f"payment-rpc auth carries only mode and admin_role, got {sorted(pcfg['auth'])}")
    check("service_name" not in pcfg.get("otel", {}), "payment-rpc does not inherit otel.service_name (it would report as rpc)")
    for k in ("http", "postgres", "log", "otel"):
        expect(pcfg.get(k), {kk: vv for kk, vv in values["config"][k].items() if kk != "service_name"}, f"payment-rpc {k} = the release's")
    expect(pcfg["worker"].get("broker"), "postgres", "payment-rpc consume reads the Postgres outbox only: worker.broker is postgres whatever the release's is")
    # the consumer group must not match the worker's lag query (worker\..*), or a payment backlog scales the worker
    group = pcfg["worker"]["consumer_group"]
    wgroup = values["config"]["worker"]["consumer_group"]
    check(group != wgroup and not group.startswith(wgroup + "."), f"payment-rpc consumer_group {group!r} must not fall under the worker's lag query ({wgroup}.*)")
    check("payment-rpc" == group, f"payment-rpc consumer_group is payment-rpc, got {group!r}")
    # Upgrade invariant (#410 slice 2): the consumer's cursor is its consumer group. The group the previous chart's
    # `payment-rpc consume` read under is payment-rpc.payment; payment-worker must read the same one, or a rolling
    # upgrade starts a new cursor (worker.start_from decides what it skips or replays).
    expect(group + ".payment", "payment-rpc.payment", "the effective consumer group of payment's consumer")
    pay = pcfg["payment"]
    expect(pay["provider"], "mock", "payment.provider (the first version's only provider)")
    check("upstream" not in pay and "api_key" not in pay and "timeout" not in pay, "payment-rpc's payment section carries nothing of rpc's (upstream / api_key / timeout)")
    for k, v in pv["config"].items():
        expect(pay.get(k), v, f"payment.{k} from paymentRpc.config.{k}")
    iv = int(pv["expire"]["intervalMinutes"])
    expect(pay["expire_interval"], f"{iv}m", "payment.expire_interval = paymentRpc.expire.intervalMinutes")
    expect(cron["spec"]["schedule"], f"*/{iv} * * * *" if iv < 60 else "0 * * * *", "expire schedule from the same intervalMinutes")
    expect(cron["spec"]["concurrencyPolicy"], "Forbid", "payment-rpc expire never overlaps itself")
    check("jwt" not in pcfg["auth"] and "oidc" not in pcfg["auth"], "payment-rpc config has no auth.jwt / auth.oidc")
    for d_ in (serve, consume, cron):
        podspec = d_["spec"]["template"]["spec"] if d_["kind"] == "Deployment" else d_["spec"]["jobTemplate"]["spec"]["template"]["spec"]
        vols = {v["name"]: v for v in podspec["volumes"]}
        expect(vols["config"]["configMap"]["name"], cm["metadata"]["name"], f"{d_['metadata']['name']} mounts payment-rpc's own ConfigMap")

    # --- env: Secret keys by secretKeyRef (optional: false), only the keys each workload needs ---
    def refs(c_):
        return {e["name"]: e["valueFrom"]["secretKeyRef"] for e in c_["env"] if "valueFrom" in e}
    for what, c_, keys in (("serve", sc, PAYMENT_SERVE_SECRET_KEYS), ("consume", cc, PAYMENT_BACKGROUND_SECRET_KEYS), ("expire", xc, PAYMENT_BACKGROUND_SECRET_KEYS)):
        r = refs(c_)
        expect(sorted(r), keys, f"payment-rpc {what} reads exactly these keys from a Secret")
        for k, ref in r.items():
            expect(ref.get("name"), own_secret, f"payment-rpc {what} {k} comes from payment-rpc's own Secret")
            expect(ref.get("key"), k, f"payment-rpc {what} {k} secretKeyRef key")
            expect(ref.get("optional"), False, f"payment-rpc {what} {k} optional: false (a missing key keeps the pod from starting)")
        names = [e["name"] for e in c_["env"]]
        check(len(names) == len(set(names)), f"payment-rpc {what} env has no duplicate names: {names}")
        env = {e["name"]: e.get("value") for e in c_["env"]}
        expect(env["APP_CONFIG"], "/etc/x-monorepo/config.yaml", f"payment-rpc {what} APP_CONFIG")
        check(not any(k.startswith("APP_AUTH_") and k != "APP_AUTH_API_KEYS" for k in env), f"payment-rpc {what} has no APP_AUTH_* env but its api key")
        # operator extraEnv first, then what the chart sets: the chart's value wins on a duplicate
        wl = pv[what]
        for e in wl.get("extraEnv", []):
            check(names.index(e["name"]) < names.index("APP_CONFIG"), f"payment-rpc {what} extraEnv {e['name']} is listed before the chart's own env")
    shared = find(docs, "Secret", "-env")
    check(shared is None or shared["metadata"]["name"] != own_secret, "payment-rpc never reads the release-wide Secret")
    if values["secrets"].get("existingSecret"):
        check(values["secrets"]["existingSecret"] != own_secret, "payment-rpc never reads secrets.existingSecret")
    check(own_secret != cr_secret_name(values, docs), "payment-rpc never reads catalog-rpc's Secret")

    if not values["secrets"]["paymentRpc"].get("existingSecret"):
        sec = one(docs, "Secret", "-payment-rpc-env")["stringData"]
        expect(sorted(sec), ["APP_AUTH_API_KEYS", "APP_PAYMENT_API_KEY", "APP_PAYMENT_MOCK_WEBHOOK_SECRET", "APP_POSTGRES_URL"],
               "payment-rpc Secret keys (payment-rpc reads three; APP_PAYMENT_API_KEY is rpc's copy of the SAME service key)")
        k = values["secrets"]["paymentRpc"]["apiKey"]
        expect(sec["APP_AUTH_API_KEYS"], f'rpc={k}:{values["config"]["auth"]["admin_role"]}', "payment-rpc accepts exactly one key, named rpc")
        expect(sec["APP_PAYMENT_API_KEY"], k, "rpc's key is the paymentRpc.apiKey value")
        expect(sec["APP_PAYMENT_MOCK_WEBHOOK_SECRET"], values["secrets"]["paymentRpc"]["mockWebhookSecret"], "the mock secret")
        expect(sec["APP_POSTGRES_URL"], values["secrets"]["postgresUrl"], "payment-rpc Postgres URL")
    else:
        check(find(docs, "Secret", "-payment-rpc-env") is None, "no payment-rpc Secret rendered when secrets.paymentRpc.existingSecret is set")

    # --- serve: probes, shutdown, port, Service, PDB, ServiceMonitor ---
    sv = pv["serve"]
    sd = sv["shutdown"]
    expect(ss["terminationGracePeriodSeconds"], sd["delaySeconds"] + sd["timeoutSeconds"] + sd["graceMarginSeconds"], "payment-rpc serve terminationGracePeriodSeconds = delay + timeout + margin")
    expect(sc["readinessProbe"]["httpGet"]["path"], "/api/readyz", "payment-rpc serve readiness path")
    expect(sc["livenessProbe"]["httpGet"]["path"], "/api/healthz", "payment-rpc serve liveness path")
    check(sc["readinessProbe"]["periodSeconds"] <= sd["delaySeconds"], "payment-rpc serve readiness period <= shutdown delay")
    expect(sc["ports"][0]["containerPort"], int(values["config"]["http"]["addr"].rsplit(":", 1)[1]), "payment-rpc serve containerPort = http.addr")
    senv = {e["name"]: e.get("value") for e in sc["env"]}
    expect(senv["APP_HTTP_SHUTDOWN_DELAY"], f'{sd["delaySeconds"]}s', "payment-rpc serve APP_HTTP_SHUTDOWN_DELAY")
    expect(senv["APP_HTTP_SHUTDOWN_TIMEOUT"], f'{sd["timeoutSeconds"]}s', "payment-rpc serve APP_HTTP_SHUTDOWN_TIMEOUT")
    expect(svc["spec"].get("type", "ClusterIP"), "ClusterIP", "the serve Service is cluster-internal: it carries the internal RPC on the webhook's port")
    expect(svc["spec"]["selector"]["app.kubernetes.io/component"], "payment-serve", "payment-rpc Service selects the serve pods")
    check("ports" not in cc and "readinessProbe" not in cc and "livenessProbe" not in cc, "payment-rpc consume has no listener, so no ports and no probes")
    check("ports" not in xc, "payment-rpc expire has no listener")
    check(cs["terminationGracePeriodSeconds"] > 18, "payment-rpc consume grace > 18s (8s handler drain + 10s router close)")
    expect(consume["spec"]["replicas"], pv["consume"]["replicaCount"], "payment-rpc consume replicas (fixed: no autoscaling, docs/operations.md)")
    check(find(docs, "HorizontalPodAutoscaler", "-payment-consume") is None and find(docs, "ScaledObject", "-payment-consume") is None, "payment-rpc consume is not autoscaled")
    pdb = find(docs, "PodDisruptionBudget", "-payment-serve")
    check((pdb is not None) == (sv["podDisruptionBudget"]["enabled"] and sv["replicaCount"] > 1), "payment-rpc serve PDB iff enabled and more than one replica")
    sm = find(docs, "ServiceMonitor", "-payment-serve")
    check((sm is not None) == sv["serviceMonitor"]["enabled"], "payment-rpc serve ServiceMonitor iff enabled")
    if sm:
        ep = sm["spec"]["endpoints"][0]
        expect(ep["path"], "/api/metrics", "payment-rpc ServiceMonitor path")
        expect(ep["port"], svc["spec"]["ports"][0]["name"], "payment-rpc ServiceMonitor port = the Service's port name")
        check(all(svc["metadata"]["labels"].get(k) == v for k, v in sm["spec"]["selector"]["matchLabels"].items()), "payment-rpc ServiceMonitor selects the serve Service")

    # --- ingress: the webhook path, nothing else, with the limits actually rendered ---
    ingresses = [d for (k, _), d in docs.items() if k == "Ingress"]
    to_serve = []
    for ing in ingresses:
        for rule in ing["spec"].get("rules", []):
            for pth in rule.get("http", {}).get("paths", []):
                if pth["backend"]["service"]["name"] == svc["metadata"]["name"]:
                    to_serve.append((ing, rule, pth))
    iv_ = pv["ingress"]
    wh = find(docs, "Ingress", "-payment-webhook")
    check((wh is not None) == bool(iv_["enabled"]), "payment-rpc webhook Ingress iff paymentRpc.ingress.enabled")
    if wh:
        check(len(to_serve) == len(iv_["hosts"]) and all(i is wh for i, _, _ in to_serve), "the serve Service is routed by the webhook Ingress and by no other Ingress")
        for _, rule, pth in to_serve:
            expect(pth["path"], WEBHOOK_PATH, "the only routed path is the webhook")
            expect(pth["pathType"], "Exact", "...matched exactly, not as a prefix (the RPC and /api/metrics live under /api/)")
        check(all(len(r.get("http", {}).get("paths", [])) == 1 for r in wh["spec"]["rules"]), "each webhook Ingress rule has exactly one path")
        check(all(r.get("host") for r in wh["spec"]["rules"]), "every webhook Ingress rule names a host")
        expect(wh["spec"].get("ingressClassName"), iv_["className"], "webhook Ingress class")
        a = wh["metadata"].get("annotations", {})
        rl = iv_["rateLimit"]
        expect(a.get("nginx.ingress.kubernetes.io/limit-rps"), str(rl["rps"]), "ingress-nginx limit-rps is rendered")
        expect(a.get("nginx.ingress.kubernetes.io/limit-burst-multiplier"), str(rl["burstMultiplier"]), "ingress-nginx limit-burst-multiplier is rendered")
        expect(a.get("nginx.ingress.kubernetes.io/limit-connections"), str(rl["connections"]), "ingress-nginx limit-connections is rendered")
        expect(a.get("nginx.ingress.kubernetes.io/proxy-body-size"), "1m", "the webhook body cap (1 MiB, docs/payment.md §7 point 7) is enforced at the ingress too")
        for k_, v_ in iv_["annotations"].items():
            expect(a.get(k_), v_, f"operator annotation {k_}")
    else:
        check(not to_serve, "no Ingress routes the serve Service while paymentRpc.ingress is off")
    # no OTHER ingress of the release may reach payment-rpc's RPC through rpc or the console: they route to their own Services only
    for ing in ingresses:
        if ing is wh:
            continue
        for rule in ing["spec"].get("rules", []):
            for pth in rule.get("http", {}).get("paths", []):
                check("payment" not in pth["backend"]["service"]["name"], f"{ing['metadata']['name']} routes to a payment-rpc Service")

    # --- NetworkPolicy: serve admits the rpc pods + the listed peers (never empty = any); the background workloads admit nobody ---
    npv = values["networkPolicy"]
    for what in ("serve", "consume", "expire"):
        pol = find(docs, "NetworkPolicy", f"-payment-{what}")
        check((pol is not None) == npv["enabled"], f"payment-rpc {what} NetworkPolicy iff networkPolicy.enabled")
        if pol:
            expect(pol["spec"]["policyTypes"], ["Ingress"], f"payment-rpc {what} NetworkPolicy is ingress-only")
            expect(pol["spec"]["podSelector"]["matchLabels"].get("app.kubernetes.io/component"), f"payment-{what}", f"payment-rpc {what} NetworkPolicy selects its pods")
            if what != "serve":
                expect(pol["spec"]["ingress"], [], f"payment-rpc {what} has no listener: deny all ingress")
    if npv["enabled"]:
        rules = one(docs, "NetworkPolicy", "-payment-serve")["spec"]["ingress"]
        check(len(rules) == 1, "payment-rpc serve NetworkPolicy has one ingress rule")
        expect(rules[0]["ports"], [{"protocol": "TCP", "port": sc["ports"][0]["containerPort"]}], "payment-rpc serve NetworkPolicy port = http.addr")
        peers = rules[0].get("from")
        extra = npv.get("paymentRpc", {}).get("ingressFrom", [])
        check(peers is not None and len(peers) == 1 + len(extra), "payment-rpc serve NetworkPolicy peers = rpc + networkPolicy.paymentRpc.ingressFrom (never empty: empty would mean any source)")
        expect(peers[0]["podSelector"]["matchLabels"].get("app.kubernetes.io/component"), "rpc", "payment-rpc serve admits the rpc pods")
        expect(peers[0]["podSelector"]["matchLabels"].get("app.kubernetes.io/instance"), serve["metadata"]["labels"]["app.kubernetes.io/instance"], "...of this release only")

    # --- rpc is told where payment-rpc is and with which key; nobody else gets a credential ---
    check("APP_PAYMENT_UPSTREAM" in renv and "APP_PAYMENT_API_KEY" in renv, "rpc is told where payment-rpc is and with which key (APP_PAYMENT_UPSTREAM, APP_PAYMENT_API_KEY)")
    expect(renv["APP_PAYMENT_UPSTREAM"].get("value"), f'http://{svc["metadata"]["name"]}:{sv["service"]["port"]}', "rpc APP_PAYMENT_UPSTREAM = the serve Service of this release")
    ref = renv["APP_PAYMENT_API_KEY"]["valueFrom"]["secretKeyRef"]
    expect(ref.get("name"), own_secret, "rpc APP_PAYMENT_API_KEY comes from payment-rpc's own Secret")
    expect(ref.get("key"), "APP_PAYMENT_API_KEY", "rpc APP_PAYMENT_API_KEY secretKeyRef key")
    expect(ref.get("optional"), False, "rpc APP_PAYMENT_API_KEY optional: false")
    check(not any(n in renv for n in ("APP_PAYMENT_MOCK_WEBHOOK_SECRET", "APP_PAYMENT_PROVIDER")), "rpc gets no provider credential")
    check(not any("secretRef" in e and e["secretRef"].get("name") == own_secret for e in rc.get("envFrom", [])), "rpc does not envFrom payment-rpc's Secret (only the one key by secretKeyRef)")
    rnames = [e["name"] for e in rc["env"]]
    check(len(rnames) == len(set(rnames)), f"rpc env has no duplicate names: {rnames}")
    # the worker, the migrate Job and the other CronJobs never see payment's Secret or a provider credential
    for (kind, name), d_ in docs.items():
        if kind not in ("Deployment", "CronJob", "Job") or "-payment-" in name or d_ is rpc:
            continue
        podspec = d_["spec"]["template"]["spec"] if kind != "CronJob" else d_["spec"]["jobTemplate"]["spec"]["template"]["spec"]
        # the worker may be told the ADDRESS (ordering.require_payment on: it refuses to start without it); never the key
        allowed = {"APP_PAYMENT_UPSTREAM"} if kind == "Deployment" and name.endswith("-worker") else set()
        for c_ in podspec["containers"] + podspec.get("initContainers", []):
            for e in c_.get("env", []):
                check(not e["name"].startswith("APP_PAYMENT_") or e["name"] in allowed, f"{name}: {e['name']} (only rpc is told about payment, and only the key and the address; the worker only the address)")
                ref_ = e.get("valueFrom", {}).get("secretKeyRef", {})
                check(ref_.get("name") != own_secret, f"{name} reads payment-rpc's Secret")
            for e in c_.get("envFrom", []):
                check(e.get("secretRef", {}).get("name") != own_secret, f"{name} envFroms payment-rpc's Secret")


def payment_secret_name(values, docs):
    ex = values["secrets"]["paymentRpc"].get("existingSecret")
    if ex:
        return ex
    sec = find(docs, "Secret", "-payment-rpc-env")
    return sec["metadata"]["name"] if sec else None


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
        # payment-rpc's own objects (<fullname>-payment-serve / -consume / -expire / -rpc / -rpc-env /
        # -webhook) end in the same words as the release's ("-expire" is the reservation CronJob, ""
        # and "-env" the shared ConfigMap and Secret): only a suffix that names payment finds them.
        if "-payment-" in name and "payment" not in suffix:
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
