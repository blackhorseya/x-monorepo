// Reads the TAP a nested `node --test --test-reporter=tap` printed and says which subtests failed (#477: `runner A
// failed` + the last 1500 bytes of 29 tests did not say which one). Pure: no process, no file. It only parses: it
// never shortens a name or an error (what to print, and where, is e2e/report.ts).

export type FailedSubtest = { path: string[]; error: string };

const INDENT = 4; // node's TAP reporter indents a nested level by four spaces

const indentOf = (line: string) => line.length - line.trimStart().length;

/**
 * Every failed LEAF subtest, with the names of the suites around it and its complete error text. A `not ok` whose only
 * cause is a failed subtest below it (`failureType: 'subtestsFailed'`) is not listed: the subtest below it is.
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
