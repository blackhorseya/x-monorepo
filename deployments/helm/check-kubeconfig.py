#!/usr/bin/env python3
"""Does every command of Taskfile.yml that can reach a cluster go through the runner, and name the kubeconfig of the task's own cluster? (#437, #444, PR #435)

    deployments/helm/check-kubeconfig.py                       # the real Taskfile.yml and deployments/helm/kind/ (`task helm-check`)
    deployments/helm/check-kubeconfig.py --taskfile <file>     # the same rules on another Taskfile (mutant 8 of target-isolation-test.sh)
    deployments/helm/check-kubeconfig.py --self-test           # the rules against commands that must be flagged, and ones that must not

THIS IS DEFENSE IN DEPTH, NOT THE GUARANTEE. On this machine ~/.kube/config can point at a production cluster. What keeps it out of
reach is deployments/helm/kind/run.sh (env -i, a scratch HOME, KUBECONFIG = the task's file, read-only variables), and that is proven by
RUNNING the real entry against a decoy caller: deployments/helm/kind/target-isolation-test.sh. A reader of shell text is always one shell
form short (PR #435 r1-r4), so this file only checks the shapes it can check exactly:

  kubectl …, helm <acting subcommand> …, kind …, deployments/helm/kind/<x>.sh …
        each SIMPLE command starts with `{{.KIND_RUN}}` (vars.KIND_RUN is exactly `deployments/helm/kind/run.sh "{{.KIND_KUBECONFIG}}"`);
        kubectl, acting helm and kind create|delete|export also carry `--kubeconfig "{{.KIND_KUBECONFIG}}"` on their own (a flag on
        another command of the same line does not count, a second one with another value is flagged, an empty one too).
        helm lint / template / repo / version / … never reach a cluster: they need no runner.
  deployments/helm/kind/<x>.sh   refuses to run outside run.sh (the `.kind-runner` check, #444); run.sh and lock.sh and the shims exist,
        lock.sh makes KUBECONFIG KIND_KUBECONFIG PATH HOME read-only.

A command is split into simple commands the way a shell does (quotes respected; ; && || | & and newline separate them). What
cannot be read that way fails closed: `$(…)`, backticks, parentheses, and a wrapper (env sh bash eval exec xargs time timeout …)
whose arguments mention kubectl / helm / kind are flagged, whatever else the command says.

The scripts under deployments/helm/kind/ are also flagged when a non-comment line changes where they point: KUBECONFIG=, unset
KUBECONFIG, KIND_KUBECONFIG=, --kubeconfig, --context, --kube-context, `kubectl config use|set|delete-context`, kind
create|delete|export, env -i, PATH=, a tool called by path, `command -p`, `hash -p`, a command word held in a variable. That list
can never be complete (r4's `read -r KUBECONFIG PATH` was not on it); the runner, not the list, is what holds.
"""
import os
import re
import sys

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))

RUN = "{{.KIND_RUN}}"
RUN_DEF = 'deployments/helm/kind/run.sh "{{.KIND_KUBECONFIG}}"'
GUARD_MARK = '[ -f "${HOME:-/nonexistent}/.kind-runner" ] || {'  # kind/*.sh refuse to run outside run.sh (#444)
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
    mentions = RUN in cmd or TOOLS.search(cmd) is not None or any(KIND_SCRIPT.match(w) for c in cmds for w in c)
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
        via_run = rest[0] == RUN  # `{{.KIND_RUN}} <command>`: the runner (env -i, scratch HOME, the task's kubeconfig only) is the door
        if via_run:
            rest = rest[1:]
            if not rest:
                why.append("%s without a command" % RUN)
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
            if not via_run:
                why.append("kubectl outside %s (the runner is what keeps the caller's kubeconfig out of reach)" % RUN)
            elif not named_right(rest):
                why.append('kubectl without --kubeconfig "%s" of its own' % EXPECT)
        elif head == "helm":
            sub = next((a for j, a in enumerate(args) if not a.startswith("-") and (j == 0 or args[j - 1] != "--kubeconfig")), "")
            if sub not in HELM_OFFLINE and not via_run:
                why.append("helm %s outside %s (the runner is what keeps the caller's kubeconfig out of reach)" % (sub, RUN))
            elif sub not in HELM_OFFLINE and not named_right(rest):
                why.append('helm %s without --kubeconfig "%s" of its own' % (sub, EXPECT))
        elif head == "kind":
            sub = next((a for a in args if not a.startswith("-")), "")
            if not via_run:
                why.append("kind %s outside %s (the runner is what keeps the caller's kubeconfig out of reach)" % (sub, RUN))
            elif sub in ("create", "delete", "export") and not named_right(rest):
                why.append('kind %s without --kubeconfig "%s" of its own' % (sub, EXPECT))
        else:
            m = KIND_SCRIPT.match(rest[0])
            if m and not m.group(1).endswith(("-test.sh", "-test.py")):
                if not via_run:
                    why.append("kind/%s must be run as `%s <script> …`" % (m.group(1), RUN))
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
    # the shim is found through PATH: a tool called by path, or looked up without the caller's PATH, never meets it
    (re.compile(r"(?:^|[\s;|&(`$\"'=])[^\s;|&()`\"']*/(?:kubectl|helm|kind)(?=[\s;|&)`\"']|$)"), "calls kubectl / helm / kind by path (the shim is only found through PATH)"),
    (re.compile(r"(?:^\s*|[;&|({]\s*|\b(?:then|do|else)\s+)(?:source|\.)\s+[\"$\w./~-]"), "sources another file (not scanned): fails closed"),
    (re.compile(r"(?:^\s*|[;&|({]\s*|\b(?:then|do|else)\s+)eval\s"), "eval: not readable, fails closed"),
    (re.compile(r"(?:\$\(|`)\s*(?:command\s+-v|which|type(?:\s+-\w+)?)\s+(?:kubectl|helm|kind)\b"), "resolves a cluster tool to a path (then calls it by path)"),
    (re.compile(r"\bcommand\s+-[a-zA-Z]*p"), "command -p looks tools up without PATH (bypasses the shims)"),
    (re.compile(r"\b(?:hash|type)\s+-[a-zA-Z]*[pP]\b|\bhash\s+-p\b"), "hash / type -p pins or resolves a tool outside PATH"),
    (re.compile(r"(?:^\s*|[;&|({`]\s*|\b(?:then|do|else)\s+)(?:exec\s+)?\"?\$\{?[A-Za-z_][A-Za-z0-9_]*\}?\"?\s+(?:--?\w+\s+)*(?:get|apply|delete|exec|rollout|create|patch|logs|describe|upgrade|install|status|uninstall|port-forward|wait|cp|run)\b"), "runs a cluster tool through a variable (not readable)"),
]


SHELL_ONLY = ("sources another file", "eval:")


# The one file a kind script may source (#462): scripts/best-effort.sh, which defines best_effort and nothing else. It is scanned with
# the same rules below (check_real), so it cannot start setting KUBECONFIG or PATH; the line must be exactly this, no other path.
ALLOWED_SOURCE = '. "$(dirname "$0")/../../../scripts/best-effort.sh"'


def judge_script(text, shell=True):
    why = []
    for n, line in enumerate(text.splitlines(), 1):
        if line.lstrip().startswith("#"):
            continue
        if shell and line.split("   #", 1)[0].strip() == ALLOWED_SOURCE:
            continue
        for rx, what in SCRIPT_RULES:
            if not shell and what.startswith(SHELL_ONLY):
                continue
            if rx.search(line):
                why.append("line %d: %s" % (n, what))
    return why


def check_taskfile(doc):
    problems = []
    run_def = (doc.get("vars") or {}).get("KIND_RUN")
    if run_def != RUN_DEF:
        problems.append("vars.KIND_RUN must be exactly %s (the runner, given the task's own kubeconfig), not %r" % (RUN_DEF, run_def))
    for name, task in sorted((doc.get("tasks") or {}).items()):
        for cmd in commands(task or {}):
            for w in judge_command(cmd):
                problems.append("task %s: %s\n    %s" % (name, w, cmd.strip().splitlines()[0][:170]))
    return problems


def check_real(taskfile, kind_dir=None):
    with open(taskfile) as f:
        problems = check_taskfile(yaml.safe_load(f))
    kind_dir = kind_dir or os.path.join(ROOT, "deployments", "helm", "kind")
    scanned = 0
    for fn in sorted(os.listdir(kind_dir)):
        if not fn.endswith((".sh", ".py")) or fn.endswith(("-test.sh", "-test.py")) or fn in ("run.sh", "lock.sh"):
            continue  # run.sh and lock.sh ARE the isolation: they set KUBECONFIG, PATH and env -i on purpose
        scanned += 1
        with open(os.path.join(kind_dir, fn)) as f:
            for w in judge_script(f.read(), shell=fn.endswith(".sh")):
                problems.append("kind/%s: %s" % (fn, w))
    best = os.path.join(ROOT, "scripts", "best-effort.sh")
    try:
        with open(best) as f:
            for w in judge_script(f.read(), shell=True):
                problems.append("scripts/best-effort.sh (sourced by the kind scripts): %s" % w)
    except OSError:
        problems.append("scripts/best-effort.sh is missing (the kind scripts source it)")
    for tool in ("kubectl", "helm"):
        p = os.path.join(kind_dir, "shim", tool)
        if not os.access(p, os.X_OK):
            problems.append("kind/shim/%s is missing or not executable" % tool)
    if not os.access(os.path.join(kind_dir, "run.sh"), os.X_OK):
        problems.append("kind/run.sh is missing or not executable")
    try:
        with open(os.path.join(kind_dir, "lock.sh")) as f:
            if not re.search(r"(?m)^readonly KUBECONFIG KIND_KUBECONFIG PATH HOME\b", f.read()):
                problems.append("kind/lock.sh must make KUBECONFIG KIND_KUBECONFIG PATH HOME read-only")
    except OSError:
        problems.append("kind/lock.sh is missing")
    # every script that acts on a cluster refuses to run outside run.sh (#444): the marker run.sh leaves in its scratch HOME
    for fn in sorted(os.listdir(kind_dir)):
        if fn.endswith(".sh") and not fn.endswith("-test.sh") and fn not in ("run.sh", "lock.sh"):
            with open(os.path.join(kind_dir, fn)) as f:
                if GUARD_MARK not in f.read():
                    problems.append("kind/%s does not refuse to run outside run.sh (the .kind-runner check is missing)" % fn)
    return problems, scanned


# cmd -> flagged?
F = '--kubeconfig "{{.KIND_KUBECONFIG}}"'
R = RUN


def wrap(cmd):
    """The same command with the runner in front of every kubectl / helm / kind that starts a simple command."""
    return re.sub(r"(?m)(^|[;&|]\s*)(kubectl|helm|kind)\b", lambda m: m.group(1) + R + " " + m.group(2), cmd)


# the flag rules, judged on commands that already go through the runner (the runner is a separate rule, below)
FLAG_CASES_BASE = [
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
]


FLAG_CASES = [
    # a flag does not stand in for the runner, and the runner does not stand in for the flag
    ("%s kubectl -n ns apply -f f" % R, True),
    ("%s kubectl %s -n ns apply -f f" % (R, F), False),
    ("kubectl %s -n ns apply -f f" % F, True),
    ("kubectl %s get pods; %s kubectl %s get ns" % (F, R, F), True),
    ("%s kubectl %s get pods; kubectl %s get ns" % (R, F, F), True),
    ("%s kubectl %s create namespace a | kubectl %s apply -f -" % (R, F, F), True),
    ("%s kubectl %s create namespace a | %s kubectl %s apply -f -" % (R, F, R, F), False),
    ("%s" % R, True),
    ("echo %s; kubectl %s get pods" % (R, F), True),
    ("helm %s upgrade --install x chart" % F, True),
    ("helm lint --strict chart", False),
    ("helm template t chart | cat", False),
    ("helm repo add kedacore https://example.test >/dev/null 2>&1 || true", False),
    ("%s helm repo add kedacore https://example.test >/dev/null 2>&1 || true" % R, False),
    ("kind get clusters", True),
    ("kind load image-archive --name a f", True),
    ("kind get kubeconfig --name a > f", True),
    ("kind create cluster --name a %s" % F, True),
    ("kind delete cluster --name a %s" % F, True),
    ("%s kind get clusters" % R, False),
    ("%s kind load image-archive --name a f" % R, False),
    ("%s kind delete cluster --name a" % R, True),
    ("%s kind delete cluster --name a %s" % (R, F), False),
    ("%s kind create cluster --name a --kubeconfig /x" % R, True),
    ("%s kind export kubeconfig --name a" % R, True),
    # the unset / export / prefix forms of r1-r4 do not matter to this rule any more: the runner makes the variables read-only
    ("unset KUBECONFIG; %s kubectl %s get pods" % (R, F), False),
    ("export KUBECONFIG=/x; %s kubectl %s get pods" % (R, F), False),
    ("KUBECONFIG=/x %s kubectl %s get pods" % (R, F), False),
    ("read -r KUBECONFIG PATH <<< \"/x /y\"; %s kubectl %s get pods" % (R, F), False),
    # what is not readable fails closed, runner or not
    ("%s sh -c 'kubectl %s get pods'" % (R, F), True),
    ("%s bash -c \"kubectl %s get pods\"" % (R, F), True),
    ("%s env -u KUBECONFIG kubectl %s get pods" % (R, F), True),
    ("%s xargs kubectl %s get" % (R, F), True),
]
CMD_CASES = [(wrap(c), f) for c, f in FLAG_CASES_BASE] + FLAG_CASES + [
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
    # scripts: through the runner, nothing else
    ("deployments/helm/kind/smoke.sh ns rel", True),
    ("{{.KIND_DIR}}/smoke.sh ns rel", True),
    ("%s {{.KIND_DIR}}/smoke.sh ns rel" % R, False),
    ("%s deployments/helm/kind/smoke.sh ns rel" % R, False),
    ("%s {{.KIND_DIR}}/smoke.sh ns rel | tee log" % R, False),
    ("true; {{.KIND_DIR}}/smoke.sh ns rel", True),
    ("%s true; {{.KIND_DIR}}/smoke.sh ns rel" % R, True),
    ("echo %s; {{.KIND_DIR}}/smoke.sh ns rel" % R, True),
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
    (ALLOWED_SOURCE + "   # best_effort (#462)\n", False),
    (ALLOWED_SOURCE + "\n", False),
    (". \"$(dirname \"$0\")/../../../scripts/other.sh\"\n", True),
    (". \"$(dirname \"$0\")/../../scripts/best-effort.sh\"\n", True),
    (ALLOWED_SOURCE + "; . ./evil.sh\n", True),
    ("true && " + ALLOWED_SOURCE + "\n", True),
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
    ke = kube_env if kube_env is not None else RUN_DEF
    d = {"vars": {"KIND_RUN": ke}, "tasks": {"t": {"cmds": cmds}}}
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
        (doc_with(["%s kubectl %s get pods" % (R, F)]), False),
        (doc_with([{"for": ["a"], "cmd": "%s kubectl %s get pods" % (R, F)}]), False),
        (doc_with(["kubectl %s get pods" % F]), True),
        (doc_with([{"task": "other"}]), False),
        (doc_with(["true"], kube_env=":"), True),
        (doc_with(["true"], kube_env='deployments/helm/kind/run.sh "other"'), True),  # the runner must be given the task's own kubeconfig
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
    kind_dir = os.path.abspath(argv[argv.index("--kind-dir") + 1]) if "--kind-dir" in argv else None
    problems, scanned = check_real(taskfile, kind_dir)
    if scanned < 8:  # the anchor: the directory scan saw the scripts it is there for
        problems.append("only %d kind scripts scanned" % scanned)
    if problems:
        print("check-kubeconfig: %d problem(s)" % len(problems))
        for p in problems:
            print("  " + p)
        return 1
    print("check-kubeconfig ok: every kubectl / helm / kind create|delete|export command of %s goes through the runner and carries its own --kubeconfig, every cluster command and kind script runs through the runner; %d kind scripts keep their guards" % (os.path.basename(taskfile), scanned))
    return 0


if __name__ == "__main__":
    sys.exit(main())
