#!/usr/bin/env python3
"""Cut the shell out of GitHub workflow / composite-action files for scripts/check-exit-swallow.sh (#465).

  extract-run-blocks.py <outdir> <file.yml>...

For every `run:` step it writes <outdir>/<n>.sh and prints `<outdir>/<n>.sh<TAB><file.yml><TAB><line of the script's first line>`
so a finding in the extracted script can be reported against the YAML line. The first line of each extracted script is a
`set` line that says what the runner's shell does (GitHub docs, "jobs.<job_id>.steps[*].shell"):
  no `shell:`            -> bash -e {0}                              no pipefail: `a | b` only reports b
  `shell: bash`          -> bash --noprofile --norc -eo pipefail {0}
  any `shell:` that spells out pipefail -> pipefail
  a shell that is not bash/sh (python, pwsh, cmd, ...) -> skipped (not shell code)
`shell:` is looked up step -> job `defaults.run.shell` -> workflow `defaults.run.shell`. ${{ ... }} becomes a placeholder word so
the script still parses. Exit 2 when PyYAML is missing or a file does not parse: a skipped file is never "clean".
"""
import os
import re
import sys

try:
    import yaml
except ImportError:
    sys.stderr.write("extract-run-blocks: PyYAML is not installed\n")
    sys.exit(2)

EXPR = re.compile(r"\$\{\{.*?\}\}", re.S)


def expr_sub(m):
    return "GHEXPR" + "\n" * m.group(0).count("\n")


def mapping(node, key):
    if isinstance(node, yaml.MappingNode):
        for k, v in node.value:
            if isinstance(k, yaml.ScalarNode) and k.value == key:
                return v
    return None


def scalar(node):
    return node.value if isinstance(node, yaml.ScalarNode) else None


def default_shell(node):
    d = mapping(node, "defaults")
    r = mapping(d, "run") if d is not None else None
    return scalar(mapping(r, "shell")) if r is not None else None


def shell_header(shell):
    """None = skip; else the `set` line the runner's shell implies."""
    if shell is None:
        return "set -e"
    first = shell.split()[0] if shell.split() else ""
    if os.path.basename(first) not in ("bash", "sh"):
        return None
    if first.endswith("bash") and len(shell.split()) == 1:
        return "set -eo pipefail"
    return "set -eo pipefail" if "pipefail" in shell else "set -e"


def steps_of(node):
    s = mapping(node, "steps")
    return s.value if isinstance(s, yaml.SequenceNode) else []


def main():
    if len(sys.argv) < 3:
        sys.stderr.write(__doc__)
        return 2
    outdir, files = sys.argv[1], sys.argv[2:]
    n = 0
    for path in files:
        try:
            with open(path, encoding="utf-8") as f:
                root = yaml.compose(f)
        except (OSError, yaml.YAMLError) as e:
            sys.stderr.write("extract-run-blocks: %s: %s\n" % (path, str(e).splitlines()[0]))
            return 2
        if root is None:
            continue
        groups = []  # (default shell, steps)
        wf_shell = default_shell(root)
        jobs = mapping(root, "jobs")
        if jobs is not None:
            for _, job in jobs.value:
                groups.append((default_shell(job) or wf_shell, steps_of(job)))
        runs = mapping(root, "runs")  # composite action
        if runs is not None:
            groups.append((None, steps_of(runs)))
        for dflt, steps in groups:
            for step in steps:
                run = mapping(step, "run")
                if not isinstance(run, yaml.ScalarNode):
                    continue
                header = shell_header(scalar(mapping(step, "shell")) or dflt)
                if header is None:
                    continue
                n += 1
                first = run.start_mark.line + 1 + (1 if run.style in ("|", ">") else 0)
                body = EXPR.sub(expr_sub, run.value)
                dest = os.path.join(outdir, "%d.sh" % n)
                with open(dest, "w", encoding="utf-8") as out:
                    out.write(header + "\n" + body + ("" if body.endswith("\n") else "\n"))
                sys.stdout.write("%s\t%s\t%d\n" % (dest, path, first))
    return 0


sys.exit(main())
