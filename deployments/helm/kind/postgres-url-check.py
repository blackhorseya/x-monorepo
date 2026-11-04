#!/usr/bin/env python3
"""Fail closed unless the rendered chart connects ONLY to what it is given, and stays inside its namespace.

    helm template ... | postgres-url-check.py --expect APP_POSTGRES_URL=<url> [--expect APP_REDIS_URL=<url> ...]

payment-upgrade.sh runs two charts in a namespace of its own, against the Postgres and Redis of that namespace, and must
never leave a workload connected to the home release's. The inputs that decide that are not enumerable by reading values
(secrets.postgresUrl / redisUrl, secrets.existingSecret, secrets.catalogRpc.existingSecret, secrets.paymentRpc.existingSecret,
every *.extraEnv, a postgres.url / redis.url in config, whatever the next chart version adds), so the check is on the
RENDERED manifests. For every container and init container of every Deployment / StatefulSet / DaemonSet / Job / CronJob /
Pod, the effective value of each --expect key is resolved the way Kubernetes does (a later `env` entry wins; `env` beats
`envFrom`; a `secretKeyRef` or `secretRef` is read from the Secret the same render produced) and must equal the expected one.

Refused, with the reason on stderr (exit 1):
  - an effective value that is not the expected one (a fully qualified name of another namespace included);
  - a source that cannot be resolved from the render: a Secret / ConfigMap the render does not contain (an
    `existingSecret`), a configMapKeyRef, fieldRef, resourceFieldRef, or a key the render does not give where the
    reference is not optional;
  - a ConfigMap whose config.yaml carries postgres.url / redis.url (a second source);
  - an Ingress, an object of a cluster-scoped kind, or an object that names a namespace of its own (it would leave the
    drill's namespace and collide with the home release);
  - a render in which no container resolves the FIRST expected key, or none of them a Job (the migrate hook): the check
    would be vacuous.
A container without a key (the console's nginx) is not a client of it and passes.
"""
import base64
import sys

import yaml

POD_KINDS = {"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "Pod"}
# Nothing the drill installs may be visible outside its namespace.
FORBIDDEN_KINDS = {
    "Ingress", "IngressClass", "Namespace", "ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition",
    "PersistentVolume", "StorageClass", "PriorityClass", "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration",
    "APIService", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "RuntimeClass", "CSIDriver", "Node",
}
CONFIG_SECTIONS = {"APP_POSTGRES_URL": "postgres", "APP_REDIS_URL": "redis"}


def pod_spec(doc):
    kind = doc.get("kind")
    if kind == "CronJob":
        return doc["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    if kind == "Pod":
        return doc["spec"]
    if kind in POD_KINDS:
        return doc["spec"]["template"]["spec"]
    return None


def secret_data(doc):
    out = {k: base64.b64decode(v).decode() for k, v in (doc.get("data") or {}).items()}
    out.update(doc.get("stringData") or {})
    return out


def resolve(c, key, where, secrets, configmaps, problems):
    """-> (value or None, source description)"""
    value, how = None, ""
    for ef in c.get("envFrom") or []:  # lowest precedence; a later entry overrides an earlier one
        for field, store, label in (("secretRef", secrets, "Secret"), ("configMapRef", configmaps, "ConfigMap")):
            if field not in ef:
                continue
            name = ef[field]["name"]
            if name not in store:
                problems.append(f'{where}: envFrom {label} "{name}" is not in the render (an existingSecret?): cannot tell what it carries')
            elif key in store[name]:
                value, how = store[name][key], f'envFrom {label} "{name}"'
    for e in c.get("env") or []:  # beats envFrom
        if e["name"] != key:
            continue
        if "value" in e:
            value, how = e["value"], "env value"
        elif "secretKeyRef" in (e.get("valueFrom") or {}):
            ref = e["valueFrom"]["secretKeyRef"]
            if ref["name"] not in secrets:
                problems.append(f'{where}: {key} comes from Secret "{ref["name"]}", which is not in the render (an existingSecret?)')
                return None, "unresolved"
            if ref["key"] not in secrets[ref["name"]]:
                if ref.get("optional"):
                    value, how = None, ""
                else:
                    problems.append(f'{where}: {key} comes from key "{ref["key"]}" of Secret "{ref["name"]}", which the render does not give')
                    return None, "unresolved"
            else:
                value, how = secrets[ref["name"]][ref["key"]], f'Secret "{ref["name"]}" key {ref["key"]}'
        else:
            problems.append(f'{where}: {key} comes from {list(e.get("valueFrom", {}))}: cannot be resolved from the render')
            return None, "unresolved"
    return value, how


def main(expect):
    secrets, configmaps, workloads, problems = {}, {}, [], []
    for d in yaml.safe_load_all(sys.stdin):
        if not d:
            continue
        kind, meta = d.get("kind"), d.get("metadata", {})
        name = meta.get("name", "")
        if kind in FORBIDDEN_KINDS:
            problems.append(f"{kind}/{name}: a {kind} is visible outside the drill's namespace")
        if meta.get("namespace"):
            problems.append(f'{kind}/{name}: names its own namespace "{meta["namespace"]}"')
        if kind == "Secret":
            secrets[name] = secret_data(d)
        elif kind == "ConfigMap":
            configmaps[name] = d.get("data") or {}
        spec = pod_spec(d)
        if spec is not None:
            workloads.append((f"{kind}/{name}", spec))

    for cname, data in configmaps.items():
        for fname, text in data.items():
            if not fname.endswith((".yaml", ".yml")):
                continue
            try:
                cfg = yaml.safe_load(text)
            except yaml.YAMLError:
                continue
            for key, section in CONFIG_SECTIONS.items():
                if key in expect and isinstance(cfg, dict) and isinstance(cfg.get(section), dict) and cfg[section].get("url"):
                    problems.append(f"ConfigMap/{cname} {fname}: {section}.url is set: a second source of {key}")

    matched, jobs = {k: 0 for k in expect}, 0
    for wname, spec in workloads:
        for c in (spec.get("initContainers") or []) + (spec.get("containers") or []):
            where = f'{wname} container {c["name"]}'
            for key, want in expect.items():
                value, how = resolve(c, key, where, secrets, configmaps, problems)
                if value is None:
                    continue
                if value != want:
                    problems.append(f"{where}: {key} (from {how}) is not the drill's own")
                else:
                    matched[key] += 1
                    jobs += key == next(iter(expect)) and wname.startswith("Job/")
                    print(f"postgres-url-check: {where}: {key} from {how} = the drill's own")
    first = next(iter(expect))
    if not problems and matched[first] == 0:
        problems.append(f"no container resolves {first}: the check would be vacuous")
    if not problems and jobs == 0:
        problems.append("the migrate hook Job is not among the containers that resolve to the drill's own database: the check does not cover the hook")
    if problems:
        for p in dict.fromkeys(problems):  # (each key reports an unresolvable source once)
            print("postgres-url-check: REFUSED: " + p, file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    args, expect = sys.argv[1:], {}
    while args:
        if len(args) < 2 or args[0] != "--expect" or "=" not in args[1]:
            sys.exit("usage: helm template ... | postgres-url-check.py --expect KEY=VALUE [--expect KEY=VALUE ...]")
        k, v = args[1].split("=", 1)
        expect[k] = v
        args = args[2:]
    if not expect:
        sys.exit("usage: helm template ... | postgres-url-check.py --expect KEY=VALUE [--expect KEY=VALUE ...]")
    main(expect)
