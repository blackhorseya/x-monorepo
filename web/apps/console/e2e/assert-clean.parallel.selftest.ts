import { describe, it, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { type NestedResult, groupAlive, killNested, runNested } from "./nested.ts";
import { explainFailure, failedSubtests, middleCut } from "./tap.ts";

// Two selftest runs at the same time, each under its own caller E2E pair (the way reviewers and parallel
// worktrees run), must both pass: the selftest brings the ports it needs and takes nothing the caller's
// pair or the other run holds (#386). A selftest with ports of its own written in would collide here.
// When one fails, the message names the failed subtest and its error (e2e/tap.ts), not the tail of the output (#477).
const consoleDir = fileURLToPath(new URL("..", import.meta.url));
// A runner takes ~10 s, up to ~40 s on a loaded machine. Its deadline is shorter than the test's own, so the runner is
// killed and waited for here, with its message, before the test's timeout could take the parent down around it.
const RUNNER_DEADLINE_MS = 90_000;
const TEST_TIMEOUT_MS = 150_000;
after(killNested);

function runner(pair: { rpc: number; web: number }, file = "e2e/assert-clean.selftest.ts", deadlineMs = RUNNER_DEADLINE_MS, onSpawn?: (pid: number) => void): Promise<NestedResult> {
  // The reporter is named, not left to the TTY check: spec on a terminal, TAP in CI (#386 — the first version matched
  // the spec summary and failed on GitHub).
  return runNested(["--test", "--test-reporter=tap", "--experimental-strip-types", file], {
    cwd: consoleDir,
    env: { ...process.env, E2E_RPC_PORT: String(pair.rpc), E2E_WEB_PORT: String(pair.web) },
    deadlineMs,
    onSpawn,
  });
}

/** Fails when the runner ran nothing or did not exit 0. `sink` takes the lines meant for stderr (the whole diagnosis); the message is the summary. */
function assertRunnerPassed(label: string, r: NestedResult, _sink: (line: string) => void = (l) => console.error(l)): void {
  assert.equal(r.timedOut, false, `runner ${label} failed: ${explainFailure(r)}`);
  assert.match(r.out, /^# pass [1-9]/m, `runner ${label} ran no tests (an empty run is not a pass):\n${r.out.slice(-800)}`);
  assert.equal(r.code, 0, `runner ${label} failed (exit ${r.code ?? r.signal}):\n${explainFailure(r)}`);
}

describe("assert-clean selftest, run in parallel", () => {
  it("two runners with different E2E pairs both exit 0", { timeout: TEST_TIMEOUT_MS }, async () => {
    const pairA = { rpc: 33131, web: 44131 };
    const pairB = { rpc: 34131, web: 45131 };
    const [a, b] = await Promise.all([runner(pairA), runner(pairB)]);
    assertRunnerPassed(`A (${pairA.rpc}/${pairA.web})`, a);
    assertRunnerPassed(`B (${pairB.rpc}/${pairB.web})`, b);
  });
});

// A red parallel run has to say WHICH subtest failed and why (#477: `runner A failed` + the last 1500 bytes of 29 tests said
// nothing). The whole diagnosis — every failed leaf, its suites, its complete error — goes to stderr between two fixed marker
// lines, where the CI log has no length problem; the assertion message is only a summary and never carries an error.
// Every case runs a real nested `node --test` over a fixture and reads what assertRunnerPassed wrote to its sink.
describe("a failed runner names every failing subtest on stderr", () => {
  const dir = mkdtempSync(join(tmpdir(), "assert-clean-parallel-"));
  after(() => rmSync(dir, { recursive: true, force: true }));
  const fixture = (name: string, body: string) => {
    const file = join(dir, name);
    writeFileSync(file, ['import { describe, it } from "node:test";', 'import assert from "node:assert/strict";', body, 'for (let i = 0; i < 40; i++) it(`passing filler subtest number ${i}`, () => {});'].join("\n"));
    return file;
  };
  const pair = { rpc: 33231, web: 44231 };
  const diagnose = (r: NestedResult) => {
    const lines: string[] = [];
    let message = "";
    try {
      assertRunnerPassed("M (33231/44231)", r, (l) => lines.push(l));
    } catch (e) {
      message = (e as Error).message;
    }
    return { message, stderr: lines.join("\n") };
  };
  const failing = (n: number, name: (i: number) => string = (i) => `many-${i}`) => Array.from({ length: n }, (_, i) => `it(${JSON.stringify(name(i + 1))}, () => assert.fail("CAUSE-AT-${i + 1}"));`).join("\n");

  it("control: a fixture whose subtests all pass is accepted and writes nothing", async () => {
    const r = await runner(pair, fixture("green.selftest.ts", ""));
    assert.equal(r.code, 0, r.out);
    const d = diagnose(r);
    assert.equal(d.message, "");
    assert.equal(d.stderr, "");
  });

  it("one failing subtest of 41: its name and error are between the markers; the message is a summary without the error", async () => {
    const r = await runner(pair, fixture("red.selftest.ts", 'it("mutant: this subtest fails on purpose", () => assert.equal(1, 2, "boom-mutant-42"));'));
    assert.notEqual(r.code, 0);
    const d = diagnose(r);
    assert.match(d.stderr, /^=== nested failures \(1\)[^\n]*===$/m);
    assert.match(d.stderr, /^=== end nested failures ===$/m);
    assert.match(d.stderr, /mutant: this subtest fails on purpose/);
    assert.match(d.stderr, /boom-mutant-42/);
    assert.match(d.message, /1 failed subtest/);
    assert.match(d.message, /stderr/, "the message must point at the stderr block");
    assert.doesNotMatch(d.message, /boom-mutant-42/, "an error in the message is a place to cut it");
  });

  it("a failing subtest two levels down: its suites and its error", async () => {
    const body = 'describe("outer suite", () => { describe("inner suite", () => { it("mutant: the deep subtest fails", () => assert.equal(1, 2, "boom-deep-7")); }); });';
    const d = diagnose(await runner(pair, fixture("deep.selftest.ts", body)));
    assert.match(d.stderr, /outer suite > inner suite > mutant: the deep subtest fails/);
    assert.match(d.stderr, /boom-deep-7/);
  });

  it("six failing subtests: every name and every error", async () => {
    const d = diagnose(await runner(pair, fixture("six.selftest.ts", failing(6, (i) => `failure-${i}`))));
    for (let i = 1; i <= 6; i++) {
      assert.match(d.stderr, new RegExp(`failure-${i}\\b`));
      assert.match(d.stderr, new RegExp(`CAUSE-AT-${i}\\b`));
    }
    for (let i = 1; i <= 6; i++) assert.match(d.message, new RegExp(`failure-${i}\\b`), "names that fit 4000 characters are all in the message");
  });

  it("38 failing subtests: the 38th name and error are there", async () => {
    const d = diagnose(await runner(pair, fixture("many.selftest.ts", failing(38))));
    assert.match(d.stderr, /nested failures \(38\)/);
    assert.match(d.stderr, /many-38\b/);
    assert.match(d.stderr, /CAUSE-AT-38\b/);
  });

  it("21 failing subtests with ~290-character names: the last name and error are there; the message omits names it cannot fit and says so", async () => {
    const d = diagnose(await runner(pair, fixture("longnames.selftest.ts", failing(21, (i) => `long-name-${String(i).padStart(2, "0")}-${"n".repeat(270)}`))));
    assert.match(d.stderr, /long-name-21-/);
    assert.match(d.stderr, /CAUSE-AT-21\b/);
    assert.match(d.message, /21 failed subtest/);
    assert.match(d.message, /omitted/, "names past 4000 characters are left out of the message, and the message says so");
    assert.ok(d.message.length < 4_000, `the message is a summary, got ${d.message.length}`);
  });

  it("a 3000-character error is printed whole: its end is there, and nothing was cut", async () => {
    const d = diagnose(await runner(pair, fixture("long.selftest.ts", 'it("long-error", () => { throw new Error(`${"x".repeat(3000)} ROOT-CAUSE-END`); });')));
    assert.ok(d.stderr.includes(`${"x".repeat(3000)} ROOT-CAUSE-END`), "the complete error must be on stderr");
    assert.doesNotMatch(d.stderr, /\[cut /);
  });

  it("a child that prints no TAP and exits 1: the tail of what it printed, and that there was no TAP", async () => {
    const file = join(dir, "no-tap.selftest.ts");
    writeFileSync(file, 'console.error("no-tap-marker: died before any test"); process.exit(1);');
    const r = await runner(pair, file);
    assert.equal(r.code, 1);
    const d = diagnose(r);
    assert.match(d.stderr, /no-tap-marker: died before any test/);
    assert.match(d.stderr, /no TAP/i);
  });

  it("a runner that never ends is killed at its deadline with its whole group; stderr has what it printed, the message says deadline", async () => {
    const file = join(dir, "hang.selftest.ts");
    // a grandchild in the runner's group, the way `pnpm exec vite` is: killing only the runner would leave it behind
    writeFileSync(file, 'import { spawn } from "node:child_process"; spawn("sleep", ["120"], { stdio: "ignore" }); console.log("hang-marker"); setInterval(() => {}, 1000);');
    let pid = 0;
    const t0 = Date.now();
    const r = await runner(pair, file, 1_500, (p) => (pid = p));
    assert.ok(Date.now() - t0 < 1_500 + 6_000, "the run must end within its deadline plus the reap wait");
    assert.equal(r.timedOut, true);
    assert.ok(pid > 0);
    for (let i = 0; i < 30 && groupAlive(pid); i++) await new Promise((res) => setTimeout(res, 100));
    assert.equal(groupAlive(pid), false, "the runner's group (its grandchild too) must be gone after the deadline");
    const d = diagnose(r);
    assert.match(d.message, /deadline/);
    assert.match(d.stderr, /hang-marker/);
  });

  it("control: a runner that ends in time is not marked timed out", async () => {
    const r = await runner(pair, fixture("green2.selftest.ts", ""), 60_000);
    assert.equal(r.timedOut, false);
    assert.equal(r.code, 0, r.out);
  });
});

// The reader of the TAP, on text: every shape of a failure the node reporter prints.
describe("failedSubtests reads node's TAP", () => {
  const block = (indent: string, lines: string[]) => lines.map((l) => indent + l).join("\n");
  const tap = [
    "TAP version 13",
    "# Subtest: outer suite",
    "    # Subtest: inner suite",
    "        # Subtest: leaf with a multi-line error",
    "        not ok 1 - leaf with a multi-line error",
    block("          ", ["---", "duration_ms: 1.5", "failureType: 'testCodeFailure'", "error: |-", "  first line", "  second line", "code: 'ERR_ASSERTION'", "..."]),
    "        # Subtest: leaf with a one-line error",
    "        not ok 2 - leaf with a one-line error",
    block("          ", ["---", "failureType: 'testCodeFailure'", "error: 'one line'", "..."]),
    "        1..2",
    "    not ok 1 - inner suite",
    block("      ", ["---", "failureType: 'subtestsFailed'", "error: '2 subtests failed'", "..."]),
    "not ok 1 - outer suite",
    block("  ", ["---", "failureType: 'subtestsFailed'", "error: '1 subtest failed'", "..."]),
    "# tests 2",
  ].join("\n");

  it("lists the leaves with their suites and errors, not the suites whose only failure is a subtest below", () => {
    assert.deepEqual(failedSubtests(tap), [
      { path: ["outer suite", "inner suite", "leaf with a multi-line error"], error: "first line\nsecond line" },
      { path: ["outer suite", "inner suite", "leaf with a one-line error"], error: "one line" },
    ]);
  });

  it("a passing run lists nothing, and a TAP with no `not ok` is explained by its tail", () => {
    assert.deepEqual(failedSubtests("ok 1 - fine\n# pass 1\n"), []);
    const m = explainFailure({ code: 1, out: `${"x".repeat(5000)}END-OF-OUTPUT` });
    assert.match(m, /no `not ok`/);
    assert.match(m, /END-OF-OUTPUT/);
    assert.ok(m.length < 2000, "the tail is bounded");
  });

  it("the message is bounded, keeps every name while they fit, and says when it cuts", () => {
    const failing = (n: number, error: string) => Array.from({ length: n }, (_, i) => `not ok ${i} - failing ${i}\n  ---\n  failureType: 'testCodeFailure'\n  error: '${error}'\n  ...`).join("\n");
    // 200 failures with short names (4 KB of names): all of them, by name
    const many = explainFailure({ code: 1, out: failing(200, "e") });
    assert.match(many, /200 failed subtest/);
    for (const i of [0, 100, 199]) assert.match(many, new RegExp(`failing ${i}\\n`));
    // 10 000 failures with 5000-character errors: bounded, and the cut is counted
    const huge = explainFailure({ code: 1, out: failing(10_000, "y".repeat(5000)) });
    assert.ok(huge.length < 15_000, `bounded, got ${huge.length}`);
    assert.match(huge, /more failed subtest\(s\) not listed/);
    assert.match(huge, /errors of the failures from #\d+ on are not shown/);
    // one error of 5000 characters: head and tail, with the cut and the original length
    const one = explainFailure({ code: 1, out: failing(1, `HEAD-${"y".repeat(5000)}-TAIL`) });
    assert.match(one, /HEAD-/);
    assert.match(one, /-TAIL/);
    assert.match(one, /\[cut \d+ of 5\d\d\d characters\]/);
  });

  it("middleCut keeps both ends and leaves a short text alone", () => {
    assert.equal(middleCut("short", 10), "short");
    const m = middleCut(`AAAA${"x".repeat(100)}ZZZZ`, 20);
    assert.ok(m.startsWith("AAAA") && m.endsWith("ZZZZ"));
    assert.match(m, /cut 88 of 108 characters/);
  });
});
