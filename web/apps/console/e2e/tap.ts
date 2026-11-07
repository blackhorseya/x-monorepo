// Reads the TAP a nested `node --test --test-reporter=tap` printed and says which subtests failed (#477: `runner A
// failed` + the last 1500 bytes of 29 tests did not say which one). Pure: no process, no file.

export type FailedSubtest = { path: string[]; error: string };

const INDENT = 4; // node's TAP reporter indents a nested level by four spaces
const MAX_ERROR_CHARS = 800;
const MAX_FAILED_LISTED = 5;
const TAIL_CHARS = 1500;

const indentOf = (line: string) => line.length - line.trimStart().length;

/**
 * Every failed LEAF subtest, with the names of the suites around it and its error text. A `not ok` whose only cause is
 * a failed subtest below it (`failureType: 'subtestsFailed'`) is not listed: the subtest below it is.
 */
export function failedSubtests(tap: string): FailedSubtest[] {
  const lines = tap.split("\n");
  const stack: string[] = [];
  const found: FailedSubtest[] = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]!;
    const sub = /^(\s*)# Subtest: (.*)$/.exec(line);
    if (sub) {
      const depth = sub[1]!.length / INDENT;
      stack.length = depth;
      stack[depth] = sub[2]!;
      continue;
    }
    const bad = /^(\s*)not ok \d+ - (.*?)(?: # (?:SKIP|TODO).*)?$/.exec(line);
    if (!bad) continue;
    const depth = bad[1]!.length / INDENT;
    // The YAML block that follows: `---` … `...` at one level deeper than the `not ok` line.
    const keyIndent = bad[1]!.length + 2;
    let failureType = "";
    let error = "";
    let j = i + 1;
    if (lines[j]?.trim() === "---") {
      for (j++; j < lines.length && lines[j]!.trim() !== "..."; j++) {
        const key = /^(\s*)(\w+):\s?(.*)$/.exec(lines[j]!);
        if (!key || key[1]!.length !== keyIndent) continue;
        if (key[2] === "failureType") failureType = key[3]!.replace(/^'|'$/g, "");
        if (key[2] === "error") {
          if (/^[|>][-+]?$/.test(key[3]!)) {
            const body: string[] = [];
            while (j + 1 < lines.length && (lines[j + 1]!.trim() === "" || indentOf(lines[j + 1]!) > keyIndent) && lines[j + 1]!.trim() !== "...") body.push(lines[++j]!.trim());
            error = body.join("\n").trim();
          } else error = key[3]!.replace(/^'|'$/g, "");
        }
      }
    }
    if (failureType === "subtestsFailed") continue;
    found.push({ path: [...stack.slice(0, depth), bad[2]!], error });
  }
  return found;
}

/** What to print under "runner X failed": the failed subtests with their errors, or — when the TAP names none — the tail of what it printed. Bounded. */
export function explainFailure(r: { code: number | null; out: string; timedOut?: boolean }): string {
  const parts: string[] = [];
  if (r.timedOut) parts.push("the runner did not finish within its deadline and was killed");
  const failed = failedSubtests(r.out);
  if (failed.length > 0) {
    parts.push(`${failed.length} failed subtest(s):`);
    for (const f of failed.slice(0, MAX_FAILED_LISTED)) parts.push(`  not ok - ${f.path.join(" > ")}\n${indent(f.error.slice(0, MAX_ERROR_CHARS) || "(no error text in the TAP)", "      ")}`);
    if (failed.length > MAX_FAILED_LISTED) parts.push(`  … and ${failed.length - MAX_FAILED_LISTED} more`);
  } else {
    parts.push(`no \`not ok\` in its output (it died before reporting, or never printed TAP); the last ${TAIL_CHARS} characters it printed:\n${r.out.slice(-TAIL_CHARS)}`);
  }
  return parts.join("\n");
}

const indent = (text: string, by: string) => text.split("\n").map((l) => by + l).join("\n");
