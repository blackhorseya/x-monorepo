import assert from "node:assert/strict";
import type { NestedResult } from "./nested.ts";
import { failedSubtests } from "./tap.ts";

// What a red nested run leaves behind. The WHOLE diagnosis goes to stderr, uncut, between two fixed marker lines: every
// failed leaf subtest with its suites and its complete error (CI's log has no length problem). The assertion message is a
// summary: how many failed, and — when the names fit — which. It never carries an error, so it cannot hide the cause by
// cutting it; and nothing here has a count or a size that decides which failure is shown.

export const BLOCK_END = "=== end nested failures ===";
const blockBegin = (n: number, label: string) => `=== nested failures (${n}) runner ${label} ===`;
const MESSAGE_NAMES_CHARS = 4_000; // all the names go in the message when together they are at most this long
const EDGE_NAMES = 3; // otherwise this many from each end
const NAME_IN_MESSAGE = 300;
const RAW_TAIL_CHARS = 20_000; // with no TAP to read: the end of what the runner printed
const indent = (text: string, by: string) => text.split("\n").map((l) => by + l).join("\n");
const clip = (text: string, max: number) => (text.length <= max ? text : `${text.slice(0, max)}…`);

/** Writes the stderr block for a runner that did not pass, and returns the summary for the assertion message. */
export function reportFailure(label: string, r: NestedResult, sink: (text: string) => void = (t) => console.error(t)): string {
  const failed = failedSubtests(r.out);
  sink(blockBegin(failed.length, label));
  if (r.timedOut) sink("the runner did not finish within its deadline and was killed");
  for (const f of failed) sink(`not ok - ${f.path.join(" > ")}\n${indent(f.error || "(no error text in the TAP)", "    ")}`);
  // A test FILE that dies (spawn failure, uncaught error before any test) is reported as one `not ok` whose error is just
  // "test failed": what killed it was printed outside the test results. So the raw tail goes into the block in that case too.
  const fileDied = failed.some((f) => f.error === "test failed");
  if (failed.length === 0 || fileDied) {
    sink(`${failed.length === 0 ? "no failed subtest to list: it printed no TAP, or died before reporting" : "a file died without test results (no TAP from it): the cause is outside the results"}; the last ${RAW_TAIL_CHARS} characters the runner printed:\n${r.out.slice(-RAW_TAIL_CHARS)}`);
  }
  sink(BLOCK_END);

  const how = r.timedOut ? "did not finish within its deadline and was killed" : `failed (exit ${r.code ?? r.signal})`;
  const lines = [`runner ${label} ${how}: ${failed.length} failed subtest(s); their complete names, suites and errors are in the stderr block "${blockBegin(failed.length, label)}" above.`];
  const names = failed.map((f) => clip(f.path.join(" > "), NAME_IN_MESSAGE));
  if (names.join("").length <= MESSAGE_NAMES_CHARS) lines.push(...names.map((n) => `  not ok - ${n}`));
  else {
    lines.push(`  the names do not fit here (${names.length}): the first ${EDGE_NAMES} and the last ${EDGE_NAMES}, the ${names.length - 2 * EDGE_NAMES} in between omitted:`);
    lines.push(...names.slice(0, EDGE_NAMES).map((n) => `  not ok - ${n}`), "  …", ...names.slice(-EDGE_NAMES).map((n) => `  not ok - ${n}`));
  }
  return lines.join("\n");
}

/** Passes when the runner exited 0 and ran at least one test; otherwise writes the stderr block and fails with the summary. */
export function assertRunnerPassed(label: string, r: NestedResult, sink?: (text: string) => void): void {
  const ranTests = /^# pass [1-9]/m.test(r.out);
  if (!r.timedOut && ranTests && r.code === 0) return;
  const summary = reportFailure(label, r, sink);
  // A clean exit that ran nothing is no pass either (a nested `node --test` that quietly did nothing).
  if (!r.timedOut && r.code === 0 && !ranTests) assert.fail(`runner ${label} ran no tests (an empty run is not a pass); what it printed is in the stderr block above`);
  assert.fail(summary);
}
