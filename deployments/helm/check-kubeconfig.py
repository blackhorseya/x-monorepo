#!/usr/bin/env python3
"""Does every command of Taskfile.yml that can reach a cluster say which kubeconfig it uses? (#437, PR #435 r1)

    deployments/helm/check-kubeconfig.py              # the real Taskfile.yml and deployments/helm/kind/ (`task helm-check` runs it)
    deployments/helm/check-kubeconfig.py --self-test  # the rules against fixtures that must be flagged, and ones that must not

The kind tasks act on a cluster through kubectl, helm, kind and kind/*.sh. On this machine ~/.kube/config can point at a
production cluster, and `kind create|delete` write a kubeconfig too (delete removes the kind-<cluster> context from
$KUBECONFIG / ~/.kube/config), so the target has to be named in the command itself. The Taskfile is read with a YAML parser
(every task: `cmds`, `preconditions[].sh`, `for` loops, `cmd:` forms), never with a text search over the file. A command is
flagged when it:

  kubectl …                          has neither `{{.KUBE_ENV}}` nor `--kubeconfig`
  helm install|upgrade|status|…      same (helm lint / template / repo / version / dependency / package stay offline: exempt)
  kind create|delete|export …        has no `--kubeconfig` (kind load / get clusters / get kubeconfig never read one: exempt)
  deployments/helm/kind/<x>.sh       has no `{{.KUBE_ENV}}` (the *-test.sh / *-test.py drills run on fakes or on clusters of their own)

and the scripts under deployments/helm/kind/ (the ones a task runs, inheriting KUBE_ENV) are flagged when a non-comment line
changes where they point: KUBECONFIG=, unset KUBECONFIG, --kubeconfig, --context, --kube-context, `kubectl config use|set|
delete-context`, kind create|delete|export, env -i. Finally `KUBE_ENV` itself must still export KUBECONFIG.
"""
import os
import re
import sys

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))

KUBE_ENV = "{{.KUBE_ENV}}"
SEP = r"(?:^|[\s;|&(`])"  # a command word starts here
PRESENCE = re.compile(r"command\s+-v\s+\S+")
KUBECTL = re.compile(SEP + r"kubectl\s")
HELM_ACTING = re.compile(SEP + r"helm\s+(?:upgrade|install|status|uninstall|rollback|get|list|history|test|-n\b|--namespace\b)")
KIND_WRITING = re.compile(SEP + r"kind\s+(?:create|delete|export)\b")
KIND_SCRIPT = re.compile(r"deployments/helm/kind/([A-Za-z0-9._-]+\.(?:sh|py))|\{\{\.KIND_DIR\}\}/([A-Za-z0-9._-]+\.(?:sh|py))")


def commands(task):
    """Every shell string a task runs: cmds (string, {cmd:}, for-loop cmd:) and preconditions' sh."""
    out = []
    for c in task.get("cmds") or []:
        if isinstance(c, str):
            out.append(c)
        elif isinstance(c, dict):
            if "cmd" in c:
                out.append(c["cmd"])
    for p in task.get("preconditions") or []:
        if isinstance(p, str):
            out.append(p)
        elif isinstance(p, dict) and "sh" in p:
            out.append(p["sh"])
    return out


def judge_command(cmd):
    """The list of reasons this command is flagged (empty = fine)."""
    why = []
    cmd = PRESENCE.sub(" ", cmd)  # `command -v kubectl` asks whether the tool exists; it reaches no cluster
    named = KUBE_ENV in cmd or "--kubeconfig" in cmd
    if KUBECTL.search(cmd) and not named:
        why.append("kubectl without {{.KUBE_ENV}} or --kubeconfig")
    if HELM_ACTING.search(cmd) and not named:
        why.append("helm (acting on a cluster) without {{.KUBE_ENV}} or --kubeconfig")
    if KIND_WRITING.search(cmd) and "--kubeconfig" not in cmd:
        why.append("kind create|delete|export without --kubeconfig")
    for m in KIND_SCRIPT.finditer(cmd):
        name = m.group(1) or m.group(2)
        if name.endswith(("-test.sh", "-test.py")):
            continue
        if KUBE_ENV not in cmd:
            why.append("kind/%s without {{.KUBE_ENV}}" % name)
    return why


SCRIPT_RULES = [
    (re.compile(r"\bKUBECONFIG\s*="), "sets KUBECONFIG"),
    (re.compile(r"\bunset\s+KUBECONFIG\b"), "unsets KUBECONFIG"),
    (re.compile(r"--kubeconfig\b"), "names another kubeconfig"),
    (re.compile(r"--(?:kube-)?context\b"), "names a context"),
    (re.compile(r"\bconfig\s+(?:use|set|delete)-context\b"), "changes the current-context"),
    (re.compile(SEP + r"kind\s+(?:create|delete|export)\b"), "kind create|delete|export"),
    (re.compile(r"\benv\s+-i\b"), "env -i drops KUBECONFIG"),
]


def judge_script(text):
    why = []
    for n, line in enumerate(text.splitlines(), 1):
        if line.lstrip().startswith("#"):
            continue
        for rx, what in SCRIPT_RULES:
            if rx.search(line):
                why.append("line %d: %s" % (n, what))
    return why


def check_taskfile(doc):
    problems = []
    kube_env = (doc.get("vars") or {}).get("KUBE_ENV")
    if not isinstance(kube_env, str) or "export KUBECONFIG=" not in kube_env:
        problems.append("vars.KUBE_ENV must export KUBECONFIG")
    for name, task in sorted((doc.get("tasks") or {}).items()):
        for cmd in commands(task or {}):
            for w in judge_command(cmd):
                problems.append("task %s: %s\n    %s" % (name, w, cmd.strip().splitlines()[0][:160]))
    return problems


def check_real():
    with open(os.path.join(ROOT, "Taskfile.yml")) as f:
        problems = check_taskfile(yaml.safe_load(f))
    kind_dir = os.path.join(ROOT, "deployments", "helm", "kind")
    scanned = 0
    for fn in sorted(os.listdir(kind_dir)):
        if not fn.endswith((".sh", ".py")) or fn.endswith(("-test.sh", "-test.py")):
            continue
        scanned += 1
        with open(os.path.join(kind_dir, fn)) as f:
            for w in judge_script(f.read()):
                problems.append("kind/%s: %s" % (fn, w))
    return problems, scanned


# cmd, flagged?  (the rules against shapes that must go red and shapes that must stay green)
CMD_CASES = [
    ("kubectl -n x get pods", True),
    ("{{.KUBE_ENV}}; kubectl -n x get pods", False),
    ("kubectl --kubeconfig f get pods", False),
    ("kubectl create namespace a | kubectl apply -f -", True),
    ("{{.KUBE_ENV}}; kubectl create namespace a | kubectl apply -f -", False),
    ("helm upgrade --install x chart -n ns", True),
    ("helm -n ns status x | head -5", True),
    ("{{.KUBE_ENV}}; helm upgrade --install x chart -n ns", False),
    ("helm lint --strict chart", False),
    ("helm template t chart | cat", False),
    ("helm repo add kedacore https://example.test >/dev/null 2>&1 || true", False),
    ("kind create cluster --name a --wait 2m", True),
    ("kind delete cluster --name a", True),
    ("kind export kubeconfig --name a", True),
    ("kind delete cluster --name a --kubeconfig \"f\"", False),
    ("kind get clusters 2>/dev/null | grep -qx a || kind create cluster --name a", True),
    ("kind get kubeconfig --name a > f", False),
    ("kind load image-archive --name a f", False),
    ("deployments/helm/kind/smoke.sh ns rel", True),
    ("{{.KIND_DIR}}/smoke.sh ns rel", True),
    ("{{.KUBE_ENV}}; {{.KIND_DIR}}/smoke.sh ns rel", False),
    ("deployments/helm/kind/negative-test.sh", False),
    ("deployments/helm/kind/upgrade-failure-test.py", False),
    ("command -v kind >/dev/null && command -v kubectl >/dev/null", False),
    # the reviewer's r2 shapes: the prefix is there, but the effective KUBECONFIG is not the file's when kubectl runs
    ("{{.KUBE_ENV}}; unset KUBECONFIG; kubectl -n ns apply -f f", True),
    ("{{.KUBE_ENV}}; export KUBECONFIG=/x; kubectl get pods", True),
    ("{{.KUBE_ENV}}; KUBECONFIG=/x kubectl get pods", True),
    ("{{.KUBE_ENV}}; env -u KUBECONFIG kubectl get pods", True),
    ("{{.KUBE_ENV}}; env KUBECONFIG=/x kubectl get pods", True),
    ("kubectl get pods; {{.KUBE_ENV}}", True),
    ("kubectl get pods && {{.KUBE_ENV}}; true", True),
    ("true; {{.KUBE_ENV}}; kubectl get pods", True),
    ("{{.KUBE_ENV}}; true; kubectl get pods", True),
    ("{{.KUBE_ENV}}; true && kubectl get pods", True),
    ("{{.KUBE_ENV}}; kubectl get pods || true", True),
    ("{{.KUBE_ENV}}; sh -c 'unset KUBECONFIG; kubectl get pods'", True),
    ("{{.KUBE_ENV}}; (unset KUBECONFIG; kubectl get pods)", True),
    ("{{.KUBE_ENV}}; kubectl get pods\nunset KUBECONFIG", True),
    ("{{.KUBE_ENV}}; helm -n ns status x | head -5", False),
    ("{{.KUBE_ENV}}; kubectl create namespace a --dry-run=client -o yaml | kubectl apply -f - >/dev/null", False),
    ("kind get clusters 2>/dev/null | grep -qx a || kind create cluster --name a --kubeconfig \"{{.KIND_KUBECONFIG}}\" --wait 2m", False),
    ("kind create cluster --name a --kubeconfig /x", True),  # the one file kind may write is the task's own
    ("echo run kubectl later", True),  # a word is a word: the rule is on the shape, a false positive costs one --kubeconfig
]
SCRIPT_CASES = [
    ("kubectl -n \"$ns\" get pods\n", False),
    ("# KUBECONFIG=elsewhere is what we do not do\nkubectl get pods\n", False),
    ("KUBECONFIG=/x kubectl get pods\n", True),
    ("export KUBECONFIG=/x\n", True),
    ("unset KUBECONFIG\n", True),
    ("kubectl --kubeconfig /x get pods\n", True),
    ("kubectl --context other get pods\n", True),
    ("helm --kube-context other status x\n", True),
    ("kubectl config use-context kind-x\n", True),
    ("kubectl config current-context\n", False),
    ("kind delete cluster --name a\n", True),
    ("env -i PATH=$PATH kubectl get pods\n", True),
]


def self_test():
    bad = 0
    for cmd, flagged in CMD_CASES:
        got = bool(judge_command(cmd))
        if got != flagged:
            bad += 1
            print("SELF-TEST FAIL command %r: flagged=%s, expected %s" % (cmd, got, flagged))
    for text, flagged in SCRIPT_CASES:
        got = bool(judge_script(text))
        if got != flagged:
            bad += 1
            print("SELF-TEST FAIL script %r: flagged=%s, expected %s" % (text, got, flagged))
    # whole documents: the forms a task command can take, and KUBE_ENV
    def doc(cmds, kube_env="export KUBECONFIG=f"):
        return {"vars": {"KUBE_ENV": kube_env}, "tasks": {"t": {"cmds": cmds}}}
    doc_cases = [
        (doc(["kubectl get pods"]), True),
        (doc([{"cmd": "kubectl get pods"}]), True),
        (doc([{"for": ["a"], "cmd": "kubectl get pods"}]), True),
        (doc(["{{.KUBE_ENV}}; kubectl get pods"]), False),
        (doc([{"for": ["a"], "cmd": "{{.KUBE_ENV}}; kubectl get pods"}]), False),
        (doc([{"task": "other"}]), False),
        (doc(["{{.KUBE_ENV}}; kubectl get pods"], kube_env=":"), True),
        ({"vars": {}, "tasks": {"t": {"cmds": ["true"]}}}, True),
        ({"vars": {"KUBE_ENV": "export KUBECONFIG=f"}, "tasks": {"t": {"cmds": ["true"], "preconditions": [{"sh": "kubectl get pods"}]}}}, True),
    ]
    for d, flagged in doc_cases:
        got = bool(check_taskfile(d))
        if got != flagged:
            bad += 1
            print("SELF-TEST FAIL document %r: flagged=%s, expected %s" % (d, got, flagged))
    n = len(CMD_CASES) + len(SCRIPT_CASES) + len(doc_cases)
    if bad:
        print("check-kubeconfig self-test: %d of %d cases wrong" % (bad, n))
        return 1
    print("check-kubeconfig self-test ok: %d cases" % n)
    return 0


def main():
    if "--self-test" in sys.argv[1:]:
        return self_test()
    problems, scanned = check_real()
    if scanned < 8:  # the anchor: the directory scan saw the scripts it is there for
        problems.append("only %d kind scripts scanned" % scanned)
    if problems:
        print("check-kubeconfig: %d problem(s)" % len(problems))
        for p in problems:
            print("  " + p)
        return 1
    print("check-kubeconfig ok: every kubectl / helm / kind / kind/*.sh command of Taskfile.yml names its kubeconfig; %d kind scripts keep the inherited one" % scanned)
    return 0


if __name__ == "__main__":
    sys.exit(main())
