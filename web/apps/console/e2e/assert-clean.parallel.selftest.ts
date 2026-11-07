import { describe, it, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { type NestedResult, groupAlive, killNested, runNested } from "./nested.ts";
import { explainFailure, failedSubtests } from "./tap.ts";

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

/** Fails when the runner ran nothing or did not exit 0. The message is what a reader of a red CI job has to go on. */
function assertRunnerPassed(name: string, pair: { rpc: number; web: number }, r: NestedResult): void {
  // First: a runner killed at its deadline also "ran no tests" or "exited non-zero"; the deadline is the cause, say it.
  assert.equal(r.timedOut, false, `runner ${name} (${pair.rpc}/${pair.web}) failed: ${explainFailure(r)}`);
  assert.match(r.out, /^# pass [1-9]/m, `runner ${name} ran no tests (an empty run is not a pass):\n${r.out.slice(-800)}`);
  assert.equal(r.code, 0, `runner ${name} (${pair.rpc}/${pair.web}) failed (exit ${r.code ?? r.signal}):\n${explainFailure(r)}`);
}

describe("assert-clean selftest, run in parallel", () => {
  it("two runners with different E2E pairs both exit 0", { timeout: TEST_TIMEOUT_MS }, async () => {
    const pairA = { rpc: 33131, web: 44131 };
    const pairB = { rpc: 34131, web: 45131 };
    const [a, b] = await Promise.all([runner(pairA), runner(pairB)]);
    assertRunnerPassed("A", pairA, a);
    assertRunnerPassed("B", pairB, b);
  });
});

// A red parallel run has to say WHICH subtest failed (#477: `runner A failed` + the last 1500 bytes of 29 tests said
// nothing). A fixture whose first subtest fails and which then passes 40 more: the name and the error of the failing
// one sit far above the last 1500 bytes of output.
describe("a failed runner names the failing subtest", () => {
  const dir = mkdtempSync(join(tmpdir(), "assert-clean-parallel-"));
  after(() => rmSync(dir, { recursive: true, force: true }));
  const fixture = (name: string, body: string) => {
    const file = join(dir, name);
    writeFileSync(file, ['import { describe, it } from "node:test";', 'import assert from "node:assert/strict";', body, 'for (let i = 0; i < 40; i++) it(`passing filler subtest number ${i}`, () => {});'].join("\n"));
    return file;
  };
  const failureOf = (r: NestedResult): string => {
    try {
      assertRunnerPassed("M", pair, r);
    } catch (e) {
      return (e as Error).message;
    }
    return "";
  };
  const pair = { rpc: 33231, web: 44231 };

  it("control: a fixture whose subtests all pass is accepted", async () => {
    const r = await runner(pair, fixture("green.selftest.ts", ""));
    assert.equal(r.code, 0, r.out);
    assert.doesNotThrow(() => assertRunnerPassed("G", pair, r));
  });

  it("mutant: one failing subtest of 41 → the message names it and carries its error", async () => {
    const r = await runner(pair, fixture("red.selftest.ts", 'it("mutant: this subtest fails on purpose", () => assert.equal(1, 2, "boom-mutant-42"));'));
    assert.notEqual(r.code, 0, "the mutant must really fail the nested run");
    assert.ok(r.out.indexOf("mutant: this subtest fails on purpose") < r.out.length - 1500, "the failing subtest must be outside the last 1500 bytes, or this proves nothing");
    const message = failureOf(r);
    assert.notEqual(message, "", "a red runner must fail the assertion");
    assert.match(message, /mutant: this subtest fails on purpose/, "the failing subtest's name is missing from the message");
    assert.match(message, /boom-mutant-42/, "the failing subtest's error is missing from the message");
  });

  it("mutant: a failing subtest two levels down → the message names the inner one and carries its error", async () => {
    const body = 'describe("outer suite", () => { describe("inner suite", () => { it("mutant: the deep subtest fails", () => assert.equal(1, 2, "boom-deep-7")); }); });';
    const r = await runner(pair, fixture("deep.selftest.ts", body));
    assert.notEqual(r.code, 0);
    assert.ok(r.out.indexOf("mutant: the deep subtest fails") < r.out.length - 1500, "the failing subtest must be outside the last 1500 bytes, or this proves nothing");
    const message = failureOf(r);
    assert.match(message, /mutant: the deep subtest fails/, "the inner subtest's name is missing from the message");
    assert.match(message, /boom-deep-7/, "the inner subtest's error is missing from the message");
  });

  it("mutant: a child that prints no TAP and exits 1 → the message says so and carries what it printed", async () => {
    const file = join(dir, "no-tap.selftest.ts");
    writeFileSync(file, 'console.error("no-tap-marker: died before any test"); process.exit(1);');
    const r = await runner(pair, file);
    assert.equal(r.code, 1);
    const message = failureOf(r);
    assert.notEqual(message, "", "a runner that died without TAP must fail the assertion");
    assert.match(message, /no-tap-marker: died before any test/, "what the child printed is missing from the message");
  });

  it("mutant: six failing subtests → every name and every error is in the message (the sixth is the root cause)", async () => {
    const body = [1, 2, 3, 4, 5, 6].map((i) => `it("failure-${i}", () => assert.fail("ROOT-CAUSE-${i}"));`).join("\n");
    const r = await runner(pair, fixture("six.selftest.ts", body));
    assert.notEqual(r.code, 0);
    const message = failureOf(r);
    for (let i = 1; i <= 6; i++) {
      assert.match(message, new RegExp(`failure-${i}\\b`), `the name of failing subtest ${i} is missing from the message`);
      assert.match(message, new RegExp(`ROOT-CAUSE-${i}\\b`), `the error of failing subtest ${i} is missing from the message`);
    }
  });

  it("mutant: an error longer than the cap keeps its cause at the end, and says it was cut", async () => {
    const body = 'it("long-error", () => { throw new Error(`${"x".repeat(3000)} ROOT-CAUSE-END`); });';
    const r = await runner(pair, fixture("long.selftest.ts", body));
    assert.notEqual(r.code, 0);
    const message = failureOf(r);
    assert.match(message, /long-error/);
    assert.match(message, /ROOT-CAUSE-END/, "the end of a long error is where the cause usually is");
    assert.match(message, /cut/, "a shortened error must say it was shortened");
  });

  it("mutant: a runner that never ends is killed at its deadline, with its whole group, and the message says so", async () => {
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
    const message = failureOf(r);
    assert.match(message, /deadline/, "the message must say the runner was killed at its deadline");
    assert.match(message, /hang-marker/, "and carry what it had printed");
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

  it("the message is bounded however many subtests fail or however long an error is", () => {
    const many = Array.from({ length: 30 }, (_, i) => `not ok ${i} - failing ${i}\n  ---\n  failureType: 'testCodeFailure'\n  error: 'e${i}'\n  ...`).join("\n");
    const m = explainFailure({ code: 1, out: many });
    assert.match(m, /30 failed subtest/);
    assert.match(m, /and 25 more/);
    const long = `not ok 1 - long\n  ---\n  failureType: 'testCodeFailure'\n  error: '${"y".repeat(5000)}'\n  ...`;
    assert.ok(explainFailure({ code: 1, out: long }).length < 1200);
  });
});
