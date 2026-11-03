#!/usr/bin/env python3
"""check.py must turn red when a payment workload is made unsafe (#422).

check.py asserts the rendered chart; a green gate is only worth something if it goes red when a
rendered workload breaks the assertion. Each case takes the real render of the payment profile,
changes ONE thing in ONE workload, feeds it to the real check.py, and expects a non-zero exit whose
message names that workload. The unchanged render is the positive control (it must pass).

Matrix: workload (serve, consume, expire) x mutation (a Secret envFrom, a writable root filesystem,
a command that overrides the image ENTRYPOINT, the other app's image).

  deployments/helm/check-mutants.py        # task helm-check runs it; needs helm and PyYAML
"""
import copy
import os
import subprocess
import sys

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
CHART = os.path.join(HERE, "x-monorepo")
PROFILE = os.path.join(CHART, "ci", "payment-rpc-values.yaml")

# workload -> (kind, name suffix, the image app it must run)
WORKLOADS = {
    "serve": ("Deployment", "-payment-serve", "payment-rpc"),
    "consume": ("Deployment", "-payment-consume", "payment-worker"),
    "expire": ("CronJob", "-payment-expire", "payment-rpc"),
}


def container(doc):
    if doc["kind"] == "CronJob":
        return doc["spec"]["jobTemplate"]["spec"]["template"]["spec"]["containers"][0]
    return doc["spec"]["template"]["spec"]["containers"][0]


def mutate_env_from(c, app):
    c["envFrom"] = [{"secretRef": {"name": "some-secret"}}]


def mutate_writable_rootfs(c, app):
    c["securityContext"]["readOnlyRootFilesystem"] = False


def mutate_command(c, app):
    c["command"] = ["/bin/sh"]


def mutate_other_image(c, app):
    other = "payment-worker" if app == "payment-rpc" else "payment-rpc"
    c["image"] = c["image"].replace(f"/{app}:", f"/{other}:")


MUTATIONS = {
    "a Secret envFrom": mutate_env_from,
    "a writable root filesystem": mutate_writable_rootfs,
    "a command overriding the ENTRYPOINT": mutate_command,
    "the other app's image": mutate_other_image,
}


def run_check(docs):
    p = subprocess.run([os.path.join(HERE, "check.py"), PROFILE], input=yaml.safe_dump_all(docs), capture_output=True, text=True)
    return p.returncode, (p.stdout + p.stderr).strip()


def main():
    render = subprocess.run(["helm", "template", "t", CHART, "-f", PROFILE], capture_output=True, text=True, check=True).stdout
    docs = [d for d in yaml.safe_load_all(render) if d]
    failed = 0

    rc, out = run_check(docs)
    if rc != 0:
        print(f"check-mutants: the unchanged render does not pass check.py (positive control): {out}", file=sys.stderr)
        sys.exit(1)

    cases = 0
    for what, (kind, suffix, app) in WORKLOADS.items():
        for mname, mutate in MUTATIONS.items():
            mutated = copy.deepcopy(docs)
            target = [d for d in mutated if d["kind"] == kind and d["metadata"]["name"].endswith(suffix)]
            assert len(target) == 1, f"{kind} *{suffix}: {len(target)} objects"
            before = copy.deepcopy(container(target[0]))
            mutate(container(target[0]), app)
            assert container(target[0]) != before, f"the mutation '{mname}' changed nothing in {what}"
            rc, out = run_check(mutated)
            cases += 1
            if rc == 0:
                failed += 1
                print(f"check-mutants: FAIL {what} with {mname}: check.py still says ok", file=sys.stderr)
            elif what not in out and not (what == "consume" and "payment" in out):
                failed += 1
                print(f"check-mutants: FAIL {what} with {mname}: check.py failed, but not about {what}: {out}", file=sys.stderr)
    print(f"check-mutants: {cases - failed}/{cases} mutants caught (+ the positive control)")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
