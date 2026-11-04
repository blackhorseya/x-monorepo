#!/usr/bin/env python3
"""Does every command of Taskfile.yml that can reach a cluster carry the kubeconfig of the task's own cluster? (#437, PR #435)

    deployments/helm/check-kubeconfig.py                       # the real Taskfile.yml and deployments/helm/kind/ (`task helm-check`)
    deployments/helm/check-kubeconfig.py --taskfile <file>     # the same rules on another Taskfile (the mutants of target-exec-test.sh)
    deployments/helm/check-kubeconfig.py --self-test           # the rules against commands that must be flagged, and ones that must not

On this machine ~/.kube/config can point at a production cluster, and `kind create|delete` write a kubeconfig too, so every
call that can act on a cluster names the file itself:

  kubectl …, helm <acting subcommand> …, kind create|delete|export …
        each SIMPLE command carries `--kubeconfig "{{.KIND_KUBECONFIG}}"` (or `--kubeconfig={{.KIND_KUBECONFIG}}`) on its own:
        a flag on another command of the same line does not count, a second --kubeconfig with another value is flagged,
        and so is an empty one. A flag beats KUBECONFIG, so nothing written around the command can undo it.
        (helm lint / template / repo / version / … never reach a cluster; kind load / get never read a kubeconfig: exempt.)
  deployments/helm/kind/<x>.sh   (the scripts call kubectl / helm without the flag; their *-test.sh / *-test.py drills run on
        fakes or on clusters of their own)
        is exactly `{{.KUBE_ENV}}; <script> …`: two simple commands and nothing else. KUBE_ENV sets KIND_KUBECONFIG and puts
        deployments/helm/kind/shim first on PATH, whose kubectl / helm add the flag to every call the script makes, at any depth.

A command is split into simple commands the way a shell does (quotes respected; ; && || | & and newline separate them). What
cannot be read that way fails closed: `$(…)`, backticks, parentheses, and a wrapper (env sh bash eval exec xargs time timeout …)
whose arguments mention kubectl / helm / kind are flagged, whatever else the command says.

The scripts under deployments/helm/kind/ are flagged when a non-comment line changes where they point: KUBECONFIG=, unset
KUBECONFIG, KIND_KUBECONFIG=, --kubeconfig, --context, --kube-context, `kubectl config use|set|delete-context`, kind
create|delete|export, env -i, PATH=. The shims must exist and be executable, and KUBE_ENV must put them first on PATH.

This is the cheap, static half. What the commands really do is judged by running them: deployments/helm/kind/target-exec-test.sh.
"""
import os
import re
import sys

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))

KUBE_ENV = "{{.KUBE_ENV}}"
EXPECT = "{{.KIND_KUBECONFIG}}"
TOOLS = re.compile(r"(?:^|[^\w.-])(kubectl|helm|kind)(?![\w-])")
KIND_SCRIPT = re.compile(r"^(?:\./)?(?:deployments/helm/kind|\{\{\.KIND_DIR\}\})/([A-Za-z0-9._-]+\.(?:sh|py))$")
ASSIGN = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
HELM_OFFLINE = {"lint", "template", "repo", "version", "dependency", "dep", "package", "show", "pull", "search", "create",
                "plugin", "completion", "env", "verify", "push", "registry", "help"}
WRAPPERS = {"env", "sh", "bash", "zsh", "dash", "eval", "exec", "command", "builtin", "xargs", "nohup", "time", "timeout",
            "source", ".", "watch", "sudo", "nice", "stdbuf", "setsid", "script", "parallel", "find", "trap", "coproc"}


def tokenize(cmd):
    """(simple commands as lists of words, opaque) — opaque when the text uses a construct this reader does not follow."""
    cmds, words, cur, quote = [], [], "", None
    opaque = False
    i, n = 0, len(cmd)
    has_cur = False

    def end_word():
        nonlocal cur, has_cur
        if has_cur:
            words.append(cur)
        cur, has_cur = "", False

    def end_cmd():
        nonlocal words
        end_word()
        if words:
            cmds.append(words)
        words = []

    while i < n:
        c = cmd[i]
        if quote == "'":
            if c == "'":
                quote = None
            else:
                cur += c
            i += 1
            continue
        if cmd.startswith("{{", i):  # a template expression is one piece
            j = cmd.find("}}", i)
            j = n if j < 0 else j + 2
            cur += cmd[i:j]
            has_cur = True
            i = j
            continue
        if quote == '"':
            if c == '"':
                quote = None
            elif c == "\\" and i + 1 < n:
                cur += cmd[i + 1]
                i += 1
            elif c == "`" or cmd.startswith("$(", i):
                opaque = True
                cur += c
            else:
                cur += c
            i += 1
            continue
        if c == "'" or c == '"':
            quote = c
            has_cur = True
        elif c == "\\" and i + 1 < n:
            cur += cmd[i + 1]
            has_cur = True
            i += 1
        elif c == "`" or cmd.startswith("$(", i) or c in "()":
            opaque = True
            cur += c
            has_cur = True
        elif c in " \t":
            end_word()
        elif c == "\n" or c == ";":
            end_cmd()
        elif c == "|":
            end_cmd()
            if cmd.startswith("||", i):
                i += 1
        elif c == "&":
            if cmd.startswith("&&", i):
                end_cmd()
                i += 1
            elif (i > 0 and cmd[i - 1] == ">") or cmd.startswith("&>", i):  # 2>&1, &>file: part of a redirection
                cur += c
                has_cur = True
            else:
                end_cmd()
        else:
            cur += c
            has_cur = True
        i += 1
    if quote:
        opaque = True
    end_cmd()
    return cmds, opaque


def flag_values(words):
    """Every --kubeconfig value among the words (None for a flag without a value)."""
    out = []
    for k, w in enumerate(words):
        if w == "--kubeconfig":
            out.append(words[k + 1] if k + 1 < len(words) else None)
        elif w.startswith("--kubeconfig="):
            out.append(w.split("=", 1)[1])
    return out


def named_right(words):
    vals = flag_values(words)
    return len(vals) >= 1 and all(v == EXPECT for v in vals)


def judge_command(cmd):
    """The list of reasons this command is flagged (empty = fine)."""
    cmds, opaque = tokenize(cmd)
    mentions = TOOLS.search(cmd) is not None or any(KIND_SCRIPT.match(w) for c in cmds for w in c)
    if not mentions:
        return []
    why = []
    if opaque:
        why.append("uses $(…), backticks, parentheses or an open quote next to kubectl / helm / kind: not readable, fails closed")
    for words in cmds:
        k = 0
        while k < len(words) and ASSIGN.match(words[k]):
            k += 1
        rest = words[k:]
        if not rest:
            continue
        head = os.path.basename(rest[0]) if "{{" not in rest[0] else rest[0]
        args = rest[1:]
        if head in WRAPPERS:
            if head == "command" and args[:1] == ["-v"]:
                continue  # asks whether a tool exists; reaches no cluster
            if TOOLS.search(" ".join(args)) or any(KIND_SCRIPT.match(a) for a in args):
                why.append("%s wraps kubectl / helm / kind or a kind script: not readable, fails closed" % head)
            continue
        if head == "kubectl":
            if not named_right(rest):
                why.append('kubectl without --kubeconfig "%s" of its own' % EXPECT)
        elif head == "helm":
            sub = next((a for j, a in enumerate(args) if not a.startswith("-") and (j == 0 or args[j - 1] != "--kubeconfig")), "")
            if sub not in HELM_OFFLINE and not named_right(rest):
                why.append('helm %s without --kubeconfig "%s" of its own' % (sub, EXPECT))
        elif head == "kind":
            sub = next((a for a in args if not a.startswith("-")), "")
            if sub in ("create", "delete", "export") and not named_right(rest):
                why.append('kind %s without --kubeconfig "%s" of its own' % (sub, EXPECT))
        else:
            m = KIND_SCRIPT.match(rest[0])
            if m and not m.group(1).endswith(("-test.sh", "-test.py")):
                if not (len(cmds) == 2 and cmds[0] == [KUBE_ENV] and cmds[1] is words):
                    why.append("kind/%s must be exactly `%s; <script> …` and nothing else" % (m.group(1), KUBE_ENV))
            elif any((KIND_SCRIPT.match(a) and not KIND_SCRIPT.match(a).group(1).endswith(("-test.sh", "-test.py"))) for a in args):
                why.append("a kind script is an argument of %s, not the command of its own" % head)
    return why


def commands(task):
    """Every shell string a task runs: cmds (string, {cmd:}, for-loop cmd:) and preconditions' sh."""
    out = []
    for c in task.get("cmds") or []:
        if isinstance(c, str):
            out.append(c)
        elif isinstance(c, dict) and "cmd" in c:
            out.append(c["cmd"])
    for p in task.get("preconditions") or []:
        if isinstance(p, str):
            out.append(p)
        elif isinstance(p, dict) and "sh" in p:
            out.append(p["sh"])
    return out


SCRIPT_RULES = [
    (re.compile(r"\bKUBECONFIG\s*="), "sets KUBECONFIG"),
    (re.compile(r"\bKIND_KUBECONFIG\s*="), "sets KIND_KUBECONFIG"),
    (re.compile(r"\bunset\b[^#\n]*\bKUBECONFIG\b"), "unsets KUBECONFIG"),
    (re.compile(r"\bunset\b[^#\n]*\bKIND_KUBECONFIG\b"), "unsets KIND_KUBECONFIG"),
    (re.compile(r"--kubeconfig\b"), "names another kubeconfig"),
    (re.compile(r"--(?:kube-)?context\b"), "names a context"),
    (re.compile(r"\bconfig\s+(?:use|set|delete)-context\b"), "changes the current-context"),
    (re.compile(r"(?:^|[\s;|&(`])kind\s+(?:create|delete|export)\b"), "kind create|delete|export"),
    (re.compile(r"\benv\s+-i\b"), "env -i drops KIND_KUBECONFIG"),
    (re.compile(r"(?:^|[\s;|&(`])(?:export\s+)?PATH\s*="), "sets PATH (the shims must stay first)"),
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
    if not isinstance(kube_env, str) or "export KUBECONFIG=" not in kube_env or "KIND_KUBECONFIG=" not in kube_env \
            or "/deployments/helm/kind/shim:$PATH" not in kube_env:
        problems.append("vars.KUBE_ENV must export KUBECONFIG and KIND_KUBECONFIG and put deployments/helm/kind/shim first on PATH")
    for name, task in sorted((doc.get("tasks") or {}).items()):
        for cmd in commands(task or {}):
            for w in judge_command(cmd):
                problems.append("task %s: %s\n    %s" % (name, w, cmd.strip().splitlines()[0][:170]))
    return problems


def check_real(taskfile):
    with open(taskfile) as f:
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
    for tool in ("kubectl", "helm"):
        p = os.path.join(kind_dir, "shim", tool)
        if not os.access(p, os.X_OK):
            problems.append("kind/shim/%s is missing or not executable" % tool)
    return problems, scanned


# cmd -> flagged?
F = '--kubeconfig "{{.KIND_KUBECONFIG}}"'
CMD_CASES = [
    # kubectl / helm / kind carry the flag themselves
    ("kubectl -n x get pods", True),
    ("kubectl %s -n x get pods" % F, False),
    ("kubectl --kubeconfig={{.KIND_KUBECONFIG}} get pods", False),
    ("kubectl --kubeconfig f get pods", True),
    ("kubectl --kubeconfig \"\" get pods", True),
    ("kubectl --kubeconfig= get pods", True),
    ("kubectl --kubeconfig get pods", True),
    ("kubectl %s --kubeconfig /x get pods" % F, True),
    ("kubectl --kubeconfig /x %s get pods" % F, True),
    ("kubectl create namespace a | kubectl apply -f -", True),
    ("kubectl %s create namespace a | kubectl apply -f -" % F, True),
    ("kubectl %s create namespace a | kubectl %s apply -f - >/dev/null" % (F, F), False),
    ("kubectl %s apply -f - >/dev/null 2>&1" % F, False),
    ("helm upgrade --install x chart -n ns", True),
    ("helm %s upgrade --install x chart -n ns" % F, False),
    ("helm %s -n ns status x | head -5" % F, False),
    ("helm -n ns status x | head -5", True),
    ("helm lint --strict chart", False),
    ("helm template t chart | cat", False),
    ("helm repo add kedacore https://example.test >/dev/null 2>&1 || true", False),
    ("kind create cluster --name a --wait 2m", True),
    ("kind delete cluster --name a", True),
    ("kind export kubeconfig --name a", True),
    ("kind delete cluster --name a %s" % F, False),
    ("kind create cluster --name a --kubeconfig /x", True),
    ("kind get clusters 2>/dev/null | grep -qx a || kind create cluster --name a", True),
    ("kind get clusters 2>/dev/null | grep -qx a || kind create cluster --name a %s --wait 2m" % F, False),
    ("kind get kubeconfig --name a > f", False),
    ("kind load image-archive --name a f", False),
    ("kind get clusters", False),
    # the flag counts only on the command it is on (lead's mutant d)
    ("kubectl get pods; kubectl %s get ns" % F, True),
    ("kubectl %s get ns; kubectl get pods" % F, True),
    ("echo %s; kubectl get pods" % F, True),
    ("kubectl get pods && kubectl %s get ns" % F, True),
    ("kubectl get pods\nkubectl %s get ns" % F, True),
    ("kubectl get pods & kubectl %s get ns" % F, True),
    ("kubectl %s get pods || kubectl get ns" % F, True),
    ("kubectl %s get pods | kubectl get ns" % F, True),
    # prefix, unset, export, order: none of them can matter when the flag is on the command, and the prefix alone is nothing
    ("{{.KUBE_ENV}}; kubectl -n ns apply -f f", True),
    ("{{.KUBE_ENV}}; unset KUBECONFIG; kubectl -n ns apply -f f", True),
    ("{{.KUBE_ENV}}; export KUBECONFIG=/x; kubectl get pods", True),
    ("kubectl get pods; {{.KUBE_ENV}}", True),
    ("unset KUBECONFIG; kubectl %s get pods" % F, False),
    ("export KUBECONFIG=/x; kubectl %s get pods" % F, False),
    ("KUBECONFIG=/x kubectl %s get pods" % F, False),
    # what is not readable fails closed
    ("sh -c 'kubectl %s get pods'" % F, True),
    ("bash -c \"kubectl %s get pods\"" % F, True),
    ("env KUBECONFIG=/x kubectl %s get pods" % F, True),
    ("env -u KUBECONFIG kubectl %s get pods" % F, True),
    ("xargs kubectl %s get" % F, True),
    ("timeout 5 kubectl %s get pods" % F, True),
    ("time kubectl %s get pods" % F, True),
    ("eval kubectl %s get pods" % F, True),
    ("exec kubectl %s get pods" % F, True),
    ("x=$(kubectl %s get pods)" % F, True),
    ("echo `kubectl %s get pods`" % F, True),
    ("(kubectl %s get pods)" % F, True),
    ("kubectl %s get pods 'unterminated" % F, True),
    # scripts: exactly the prefix and the script
    ("deployments/helm/kind/smoke.sh ns rel", True),
    ("{{.KIND_DIR}}/smoke.sh ns rel", True),
    ("{{.KUBE_ENV}}; {{.KIND_DIR}}/smoke.sh ns rel", False),
    ("{{.KUBE_ENV}}; deployments/helm/kind/smoke.sh ns rel", False),
    ("{{.KUBE_ENV}}; {{.KIND_DIR}}/smoke.sh ns rel | tee log", True),
    ("{{.KUBE_ENV}}; true; {{.KIND_DIR}}/smoke.sh ns rel", True),
    ("{{.KUBE_ENV}}; {{.KIND_DIR}}/smoke.sh ns rel; unset KUBECONFIG", True),
    ("true; {{.KUBE_ENV}}; {{.KIND_DIR}}/smoke.sh ns rel", True),
    ("{{.KIND_DIR}}/smoke.sh ns rel; {{.KUBE_ENV}}", True),
    ("{{.KUBE_ENV}}; unset KUBECONFIG {{.KIND_DIR}}/smoke.sh", True),
    ("deployments/helm/kind/negative-test.sh", False),
    ("deployments/helm/kind/upgrade-failure-test.py", False),
    ("command -v kind >/dev/null && command -v kubectl >/dev/null", False),
    ("echo run kubectl later", False),
    ("mkdir -p \"{{.BIN_DIR}}/kind\"", False),
    ("rm -f \"{{.KIND_KUBECONFIG}}\"", False),
]
SCRIPT_CASES = [
    ("kubectl -n \"$ns\" get pods\n", False),
    ("# KUBECONFIG=elsewhere is what we do not do\nkubectl get pods\n", False),
    ("KUBECONFIG=/x kubectl get pods\n", True),
    ("export KUBECONFIG=/x\n", True),
    ("KIND_KUBECONFIG=/x\n", True),
    ("unset KUBECONFIG\n", True),
    ("unset KIND_KUBECONFIG\n", True),
    ("unset -v KUBECONFIG\n", True),
    ("kubectl --kubeconfig /x get pods\n", True),
    ("kubectl --context other get pods\n", True),
    ("helm --kube-context other status x\n", True),
    ("kubectl config use-context kind-x\n", True),
    ("kubectl config current-context\n", False),
    ("kind delete cluster --name a\n", True),
    ("env -i PATH=$PATH kubectl get pods\n", True),
    ("PATH=/usr/bin kubectl get pods\n", True),
    ("export PATH=/usr/bin:$PATH\n", True),
    ("echo \"$PATH\"\n", False),
    # reaching a tool without the shim (r3: an absolute path kept both gates green)
    ("/opt/homebrew/bin/kubectl get ns\n", True),
    ("/usr/local/bin/helm -n x status r\n", True),
    ("./kubectl get ns\n", True),
    ("x=$(/opt/homebrew/bin/kubectl get ns)\n", True),
    ("echo ns | xargs /opt/homebrew/bin/kubectl get\n", True),
    ("bash -c '/opt/homebrew/bin/kubectl get ns'\n", True),
    ("KUBECTL=/opt/homebrew/bin/kubectl\n", True),
    ("command -p kubectl get ns\n", True),
    ("command -v kubectl >/dev/null\n", False),
    ("hash -p /opt/homebrew/bin/kubectl kubectl\n", True),
    ("\"$K\" get ns\n", True),
    ("$K get ns\n", True),
    ("exec $K apply -f f\n", True),
    ("kubectl get ns --dry-run=client -o name | grep -v /kubectl\n", True),  # a bare /kubectl word: fails closed, one rewrite costs nothing
    ("kubectl=$(command -v kubectl)\n", True),
    ("k=$(which helm)\n", True),
    ("source ./lib.sh\n", True),
    (". \"$here/lib.sh\"\n", True),
    ("eval \"$cmd\"\n", True),
    ("  printf '%s' \"$(sed -n '1s/^HTTP[^ ]* \\([0-9]*\\).*/\\1/p' <<<\"$out\")\"\n", False),
    ("if x; then . ./lib.sh; fi\n", True),
    ("echo a | sed 's/a/b/'\n", False),
    ("git archive \"$tag\" deployments/helm/x-monorepo | tar -x -C \"$tmp/old\"\n", False),
    ("helm upgrade --install r \"$chart\" -n \"$ns\" --wait\n", False),
    ("xargs kubectl get ns\n", False),
    ("bash -c 'kubectl get ns x'\n", False),
    ("timeout 5 kubectl get ns\n", False),
    ("k() { kubectl \"$@\"; }\n", False),
    ("k() { /opt/homebrew/bin/kubectl \"$@\"; }\n", True),
    ("kubectl -n \"$ns\" get deploy/kubectl-demo\n", False),
]


def doc_with(cmds, kube_env=None, extra=None):
    ke = kube_env if kube_env is not None else 'export KUBECONFIG=f KIND_KUBECONFIG=f PATH="{{.ROOT_DIR}}/deployments/helm/kind/shim:$PATH"'
    d = {"vars": {"KUBE_ENV": ke}, "tasks": {"t": {"cmds": cmds}}}
    if extra:
        d["tasks"]["t"].update(extra)
    return d


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
    doc_cases = [
        (doc_with(["kubectl get pods"]), True),
        (doc_with([{"cmd": "kubectl get pods"}]), True),
        (doc_with([{"for": ["a"], "cmd": "kubectl get pods"}]), True),
        (doc_with(["kubectl %s get pods" % F]), False),
        (doc_with([{"for": ["a"], "cmd": "kubectl %s get pods" % F}]), False),
        (doc_with([{"task": "other"}]), False),
        (doc_with(["true"], kube_env=":"), True),
        (doc_with(["true"], kube_env='export KUBECONFIG=f KIND_KUBECONFIG=f'), True),  # no shim on PATH
        ({"vars": {}, "tasks": {"t": {"cmds": ["true"]}}}, True),
        (doc_with(["true"], extra={"preconditions": [{"sh": "kubectl get pods"}]}), True),
        (doc_with(["true"], extra={"preconditions": [{"sh": "command -v kubectl"}]}), False),
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
    argv = sys.argv[1:]
    if "--self-test" in argv:
        return self_test()
    taskfile = os.path.join(ROOT, "Taskfile.yml")
    if "--taskfile" in argv:
        taskfile = os.path.abspath(argv[argv.index("--taskfile") + 1])
    problems, scanned = check_real(taskfile)
    if scanned < 8:  # the anchor: the directory scan saw the scripts it is there for
        problems.append("only %d kind scripts scanned" % scanned)
    if problems:
        print("check-kubeconfig: %d problem(s)" % len(problems))
        for p in problems:
            print("  " + p)
        return 1
    print("check-kubeconfig ok: every kubectl / helm / kind create|delete|export command of %s carries its own --kubeconfig, every kind script runs behind KUBE_ENV; %d kind scripts keep it" % (os.path.basename(taskfile), scanned))
    return 0


if __name__ == "__main__":
    sys.exit(main())
