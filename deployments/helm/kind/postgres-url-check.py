#!/usr/bin/env python3
"""Fail closed unless EVERY container the chart renders gets exactly one Postgres URL: the one given.

    helm template ... | postgres-url-check.py <expected-url>

payment-upgrade.sh runs the previous chart and then this one against a database of its own and must never leave a
workload pointed at the release's own database. The inputs are not enumerable by reading values (secrets.postgresUrl,
secrets.existingSecret, secrets.catalogRpc.existingSecret, secrets.paymentRpc.existingSecret, every *.extraEnv, a
postgres.url in config, a chart version that adds another one), so the check is on the RENDERED manifests: for every
container and init container of every Deployment / StatefulSet / DaemonSet / Job / CronJob / Pod, the effective
APP_POSTGRES_URL is resolved the way Kubernetes does (a later `env` entry wins; `env` beats `envFrom`; a `secretKeyRef`
or `secretRef` is read from the Secret the same render produced) and must equal <expected-url>.

Refused, with the reason on stderr (exit 1):
  - an effective value that is not <expected-url>;
  - a source that cannot be resolved from the render: a Secret / ConfigMap the render does not contain (an
    `existingSecret`), a configMapKeyRef, fieldRef, resourceFieldRef, or a value the render does not contain a key for
    where the reference is not optional;
  - a ConfigMap whose config.yaml carries postgres.url (the file is below the env in precedence, but it is a second
    source and nothing here proves the env always wins);
  - a render with no container that resolves to <expected-url> at all, or none of them a Job (the migrate hook): the
    check would be vacuous.
A container with no APP_POSTGRES_URL at all (the console's nginx) is not a Postgres client and passes.
"""
import base64
import sys

import yaml

KEY = "APP_POSTGRES_URL"
POD_KINDS = {"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "Pod"}


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
    out = {}
    for k, v in (doc.get("data") or {}).items():
        out[k] = base64.b64decode(v).decode()
    for k, v in (doc.get("stringData") or {}).items():
        out[k] = v
    return out


def main(expected):
    secrets, configmaps, workloads = {}, {}, []
    for d in yaml.safe_load_all(sys.stdin):
        if not d:
            continue
        name = d.get("metadata", {}).get("name", "")
        if d.get("kind") == "Secret":
            secrets[name] = secret_data(d)
        elif d.get("kind") == "ConfigMap":
            configmaps[name] = d.get("data") or {}
        spec = pod_spec(d)
        if spec is not None:
            workloads.append((f'{d["kind"]}/{name}', spec))

    problems, matched, jobs = [], 0, 0
    for cname, data in configmaps.items():
        for fname, text in data.items():
            if fname.endswith((".yaml", ".yml")):
                try:
                    cfg = yaml.safe_load(text)
                except yaml.YAMLError:
                    continue
                if isinstance(cfg, dict) and isinstance(cfg.get("postgres"), dict) and cfg["postgres"].get("url"):
                    problems.append(f"ConfigMap/{cname} {fname}: postgres.url is set: a second source of the Postgres URL")

    for wname, spec in workloads:
        for c in (spec.get("initContainers") or []) + (spec.get("containers") or []):
            where = f'{wname} container {c["name"]}'
            value, how = None, ""
            # envFrom first (lowest precedence); a later list entry overrides an earlier one
            for ef in c.get("envFrom") or []:
                if "secretRef" in ef:
                    ref = ef["secretRef"]
                    if ref["name"] not in secrets:
                        problems.append(f'{where}: envFrom Secret "{ref["name"]}" is not in the render (an existingSecret?): cannot tell what it carries')
                        continue
                    if KEY in secrets[ref["name"]]:
                        value, how = secrets[ref["name"]][KEY], f'envFrom Secret "{ref["name"]}"'
                elif "configMapRef" in ef:
                    ref = ef["configMapRef"]
                    if ref["name"] not in configmaps:
                        problems.append(f'{where}: envFrom ConfigMap "{ref["name"]}" is not in the render: cannot tell what it carries')
                        continue
                    if KEY in configmaps[ref["name"]]:
                        value, how = configmaps[ref["name"]][KEY], f'envFrom ConfigMap "{ref["name"]}"'
            # then env, which beats envFrom
            for e in c.get("env") or []:
                if e["name"] != KEY:
                    continue
                if "value" in e:
                    value, how = e["value"], "env value"
                elif "valueFrom" in e and "secretKeyRef" in e["valueFrom"]:
                    ref = e["valueFrom"]["secretKeyRef"]
                    if ref["name"] not in secrets:
                        problems.append(f'{where}: {KEY} comes from Secret "{ref["name"]}", which is not in the render (an existingSecret?)')
                        value, how = "<unresolved>", "unresolved"
                    elif ref["key"] not in secrets[ref["name"]]:
                        if ref.get("optional"):
                            value, how = None, ""
                        else:
                            problems.append(f'{where}: {KEY} comes from key "{ref["key"]}" of Secret "{ref["name"]}", which the render does not give')
                            value, how = "<unresolved>", "unresolved"
                    else:
                        value, how = secrets[ref["name"]][ref["key"]], f'Secret "{ref["name"]}" key {ref["key"]}'
                else:
                    problems.append(f'{where}: {KEY} comes from {list(e.get("valueFrom", {}))}: cannot be resolved from the render')
                    value, how = "<unresolved>", "unresolved"
            if value is None:
                continue
            if value == "<unresolved>":
                continue
            if value != expected:
                problems.append(f'{where}: {KEY} (from {how}) is not the run\'s database URL')
            else:
                matched += 1
                jobs += wname.startswith("Job/")
                print(f"postgres-url-check: {where}: {KEY} from {how} = the run's database")
    if matched == 0 and not problems:
        problems.append("no container resolves to the run's database URL: the check would be vacuous")
    if not problems and jobs == 0:
        problems.append("the migrate hook Job is not among the containers that resolve to the run's database: the check does not cover the hook")
    if problems:
        for p in problems:
            print("postgres-url-check: REFUSED: " + p, file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: helm template ... | postgres-url-check.py <expected-url>")
    main(sys.argv[1])
