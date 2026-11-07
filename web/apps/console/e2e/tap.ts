// Reads the TAP a nested `node --test --test-reporter=tap` printed and says which subtests failed (#477: `runner A
// failed` + the last 1500 bytes of 29 tests did not say which one). Pure: no process, no file.

export type FailedSubtest = { path: string[]; error: string };

const INDENT = 4; // node's TAP reporter indents a nested level by four spaces
// The bounds of a message. Names come first and are never dropped for a count: every failed subtest is listed, each name
// up to NAME_CHARS. Only when the names alone pass NAMES_BUDGET (hundreds of failures) is the rest cut, and the cut is
// said with its count. The errors share ERRORS_BUDGET: each gets an equal share (at least ERROR_MIN, at most ERROR_MAX),
// and one longer than its share keeps its head AND its tail — the cause is usually at the end — with the cut marked.
// Worst case: NAMES_BUDGET + ERRORS_BUDGET + the markers, under 15 KB.
const NAME_CHARS = 300;
const NAMES_BUDGET = 6_000;
const ERRORS_BUDGET = 6_000;
const LINE_OVERHEAD = 12; // "  not ok - " and the newline, counted so the budget is the real size
const ERROR_MIN = 160;
const ERROR_MAX = 1_200;
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

/** `text` cut to `max` characters: its head and its tail, with the cut and the original length said in between. */
export function middleCut(text: string, max: number): string {
  if (text.length <= max) return text;
  const head = Math.ceil(max / 2);
  const tail = Math.floor(max / 2);
  return `${text.slice(0, head)}\n… [cut ${text.length - max} of ${text.length} characters] …\n${text.slice(text.length - tail)}`;
}

/** What to print under "runner X failed": every failed subtest by name with its error (bounded as above), or — when the TAP names none — the tail of what it printed. */
export function explainFailure(r: { code: number | null; out: string; timedOut?: boolean }): string {
  const parts: string[] = [];
  if (r.timedOut) parts.push("the runner did not finish within its deadline and was killed");
  const failed = failedSubtests(r.out);
  if (failed.length === 0) {
    parts.push(`no \`not ok\` in its output (it died before reporting, or never printed TAP); the last ${TAIL_CHARS} characters it printed:\n${r.out.slice(-TAIL_CHARS)}`);
    return parts.join("\n");
  }
  const listed: FailedSubtest[] = [];
  let used = 0;
  for (const f of failed) {
    used += Math.min(f.path.join(" > ").length, NAME_CHARS) + LINE_OVERHEAD;
    if (listed.length > 0 && used > NAMES_BUDGET) break;
    listed.push(f);
  }
  // Errors of the first `withError` failures only: ERROR_MIN each is the least that says anything, so more would pass the budget.
  const withError = Math.min(listed.length, Math.floor(ERRORS_BUDGET / ERROR_MIN));
  const share = Math.min(ERROR_MAX, Math.floor(ERRORS_BUDGET / withError));
  parts.push(`${failed.length} failed subtest(s):`);
  listed.forEach((f, i) => {
    const name = `  not ok - ${middleCut(f.path.join(" > "), NAME_CHARS)}`;
    parts.push(i < withError ? `${name}\n${indent(middleCut(f.error || "(no error text in the TAP)", share), "      ")}` : name);
  });
  if (withError < listed.length) parts.push(`  (the errors of the failures from #${withError + 1} on are not shown: the errors budget of ${ERRORS_BUDGET} characters is used)`);
  if (listed.length < failed.length) parts.push(`  … ${failed.length - listed.length} more failed subtest(s) not listed (the names alone passed ${NAMES_BUDGET} characters); the first is "${middleCut(failed[listed.length]!.path.join(" > "), 80)}"`);
  return parts.join("\n");
}

const indent = (text: string, by: string) => text.split("\n").map((l) => by + l).join("\n");
