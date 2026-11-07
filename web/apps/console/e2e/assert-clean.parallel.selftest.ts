import { type ChildProcess, spawn } from "node:child_process";
import { describe, it, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

// Two selftest runs at the same time, each under its own caller E2E pair (the way reviewers and parallel
// worktrees run), must both pass: the selftest brings the ports it needs and takes nothing the caller's
// pair or the other run holds (#386). A selftest with ports of its own written in would collide here.
const consoleDir = fileURLToPath(new URL("..", import.meta.url));
const children: ChildProcess[] = [];
const killGroup = (c: ChildProcess) => {
  try {
    if (c.pid) process.kill(-c.pid, "SIGKILL");
  } catch (e) {
    // ESRCH: the group is gone. macOS answers EPERM for a group whose members have all exited but are not yet
    // reaped, so EPERM counts as gone only once the child we started has exited; anything else is a real failure.
    const code = (e as NodeJS.ErrnoException).code;
    const exited = c.exitCode !== null || c.signalCode !== null;
    if (code !== "ESRCH" && !(code === "EPERM" && exited)) throw e;
  }
};
after(() => children.forEach(killGroup));

type RunResult = { code: number | null; out: string };
function runner(pair: { rpc: number; web: number }, file = "e2e/assert-clean.selftest.ts"): Promise<RunResult> {
  return new Promise((resolve, reject) => {
    // Inside `node --test`, NODE_TEST_CONTEXT makes a nested `node --test` run quietly in-process and exit 0
    // without running anything: a passing control that tested nothing. Strip it, and check below that the
    // runner really ran its tests. The reporter is named, not left to the TTY check: spec on a terminal,
    // TAP in CI (#386 — the first version matched the spec summary and failed on GitHub).
    const { NODE_TEST_CONTEXT: _context, ...parentEnv } = process.env;
    const child = spawn("node", ["--test", "--test-reporter=tap", "--experimental-strip-types", file], {
      cwd: consoleDir,
      env: { ...parentEnv, E2E_RPC_PORT: String(pair.rpc), E2E_WEB_PORT: String(pair.web) },
      stdio: ["ignore", "pipe", "pipe"],
      detached: true,
    });
    children.push(child);
    let out = "";
    child.stdout!.on("data", (d) => (out += d));
    child.stderr!.on("data", (d) => (out += d));
    child.once("error", reject);
    child.once("exit", (code) => resolve({ code, out }));
  });
}

/** Fails when the runner ran nothing or did not exit 0. The message is what a reader of a red CI job has to go on. */
function assertRunnerPassed(name: string, pair: { rpc: number; web: number }, r: RunResult): void {
  assert.match(r.out, /^# pass [1-9]/m, `runner ${name} ran no tests (an empty run is not a pass):\n${r.out.slice(-800)}`);
  assert.equal(r.code, 0, `runner ${name} (${pair.rpc}/${pair.web}) failed:\n${r.out.slice(-1500)}`);
}

describe("assert-clean selftest, run in parallel", () => {
  it("two runners with different E2E pairs both exit 0", async () => {
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
  const failureOf = (r: RunResult): string => {
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
});
