import { describe, it, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "node:net";
import { randomInt } from "node:crypto";
import { type NestedResult, groupAlive, killNested, runNested } from "./nested.ts";
import { assertRunnerPassed } from "./report.ts";
import { failedSubtests } from "./tap.ts";
import { derivePorts } from "./ports.ts";
import { type Pair, privatePair, randomPair } from "./private-pair.ts";

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

  // A file that prints its cause first, then a lot of noise, then dies. The file flushes before it exits: one that exit()s right
  // after a big async write loses what is past the pipe buffer by itself (CI run 42360256259 showed 12 KB of ~33 KB for that reason,
  // not because of node 22's runner or this capture; with the flush the same runner forwards 35197 characters on CI).
  it("a file that prints its cause first, then 33000 characters of noise, then dies: the early cause is on stderr (no 20000-character tail)", async (t) => {
    const file = join(dir, "early.selftest.ts");
    writeFileSync(file, 'let all = "EARLY-CAUSE: import failed here\\n"; for (let i = 0; i < 600; i++) all += `noise line ${i} ${"z".repeat(40)}\\n`; process.stderr.write(all, () => process.exit(1));');
    const r = await runner(pair, file);
    assert.equal(r.code, 1);
    t.diagnostic(`the runner forwarded ${r.out.length} characters of the ~33000 the file printed`);
    const at = r.out.indexOf("EARLY-CAUSE");
    assert.ok(at >= 0, `the runner did not forward the dead file's output at all, so this proves nothing. runner output: ${JSON.stringify(r.out.slice(0, 400))}`);
    assert.ok(at < r.out.length - 20_000, `EARLY-CAUSE must lie outside the last 20000 characters, or this proves nothing (runner output: ${r.out.length} characters)`);
    assert.match(diagnose(r).stderr, /EARLY-CAUSE: import failed here/, "the early output of a dead file was cut");
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

  it("the default sink is stderr: a child that calls assertRunnerPassed prints the block there", async () => {
    const tapFile = join(dir, "38.tap");
    writeFileSync(tapFile, (await runner(pair, fixture("many2.selftest.ts", failing(38)))).out);
    const wrapper = join(dir, "wrap.selftest.ts");
    const report = new URL("./report.ts", import.meta.url).href;
    writeFileSync(wrapper, `import { readFileSync } from "node:fs"; import { assertRunnerPassed } from ${JSON.stringify(report)}; assertRunnerPassed("W", { code: 1, signal: null, timedOut: false, out: readFileSync(${JSON.stringify(tapFile)}, "utf8") });`);
    const w = await runNested(["--experimental-strip-types", wrapper], { cwd: consoleDir, env: process.env, deadlineMs: 60_000 });
    assert.notEqual(w.code, 0);
    assert.match(w.out, /^=== nested failures \(38\) runner W ===$/m);
    assert.match(w.out, /CAUSE-AT-38\b/);
    assert.match(w.out, /^=== end nested failures ===$/m);
  });

  it("a clean exit that ran no tests is not a pass", () => {
    const quiet: NestedResult = { code: 0, signal: null, timedOut: false, closeTimedOut: false, out: "TAP version 13\n1..0\n# tests 0\n# pass 0\n# fail 0\n" };
    assert.throws(() => assertRunnerPassed("Q", quiet, () => {}), /ran no tests/);
  });

  // nested.ts must not end a run while the child's output is still in the pipes (#477: only 12 KB of a dead file's output reached
  // the CI log — node 22's runner, or this capture?). These are plain children, no node runner in between.
  describe("runNested reads everything a child wrote", () => {
    const ONE_RUN = { cwd: tmpdir(), env: process.env, deadlineMs: 20_000 };
    // The child exits in the flush callback: everything is in the pipe, but the parent has not necessarily read it all when `exit` fires.
    // (Not `exit(1)` straight after write(): that child loses what is past the 64 KiB pipe buffer by itself — measured on macOS/node 26,
    // 65536 of 262144 bytes at `exit` AND at `close` — which says nothing about the capture.)
    for (const stream of ["stdout", "stderr"] as const) {
      it(`256 KiB written to ${stream} and flushed, then exit(1): every byte arrives`, async () => {
        const file = join(dir, `big-${stream}.cjs`);
        writeFileSync(file, `process.${stream}.write("x".repeat(262144), () => process.exit(1));`);
        for (let i = 0; i < 5; i++) {
          const r = await runNested([file], ONE_RUN);
          assert.equal(r.code, 1);
          assert.equal(Buffer.byteLength(r.out), 262_144, `round ${i}: received ${Buffer.byteLength(r.out)} of 262144 bytes`);
        }
      });
    }

    it("a child that exits while something it started still holds its pipes: close does not come, the run says so within closeMs, and the group is gone", async () => {
      const file = join(dir, "holder.cjs");
      writeFileSync(file, 'require("node:child_process").spawn("sleep", ["60"], { stdio: ["ignore", "inherit", "inherit"] }).unref(); console.log("holder-exiting"); process.exit(0);');
      let pid = 0;
      const t0 = Date.now();
      try {
        const r = await runNested([file], { ...ONE_RUN, closeMs: 1_000, onSpawn: (p) => (pid = p) });
        assert.ok(Date.now() - t0 < 1_000 + 6_000, "the run must end within closeMs plus the reap wait");
        assert.equal(r.closeTimedOut, true, "the output never closed, and the run did not say so");
        assert.match(r.out, /holder-exiting/);
        for (let i = 0; i < 30 && groupAlive(pid); i++) await new Promise((res) => setTimeout(res, 100));
        assert.equal(groupAlive(pid), false, "what the child started must not outlive the run");
      } finally {
        try {
          if (pid) process.kill(-pid, "SIGKILL"); // our own child's group, whatever the assertions above found
        } catch {
          // already gone
        }
      }
    });

    it("assertRunnerPassed: a run whose output did not close is no pass, even at exit 0", () => {
      const r: NestedResult = { code: 0, signal: null, timedOut: false, closeTimedOut: true, out: "TAP version 13\n# pass 5\n# fail 0\n" };
      assert.throws(() => assertRunnerPassed("C", r, () => {}), /output did not close/);
    });
  });

  // A green run writes nothing to stderr. The fault selftest is the one that once did (reportFailure as an argument of assert.match runs every time).
  it("the fault selftest, all green, writes no nested-failures block", { timeout: TEST_TIMEOUT_MS }, async () => {
    const r = await runNested(["--test", "--test-reporter=tap", "--experimental-strip-types", "e2e/assert-clean.fault.selftest.ts"], { cwd: consoleDir, env: process.env, deadlineMs: RUNNER_DEADLINE_MS });
    assert.equal(r.code, 0, r.out.slice(-1500));
    assert.match(r.out, /^# pass [1-9]/m);
    assert.doesNotMatch(r.out, /=== nested failures/, "a green run printed a failure block");
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

  it("a passing run lists nothing, and an error is returned whole however long", () => {
    assert.deepEqual(failedSubtests("ok 1 - fine\n# pass 1\n"), []);
    const long = `not ok 1 - long\n  ---\n  failureType: 'testCodeFailure'\n  error: '${"y".repeat(5000)}'\n  ...`;
    assert.equal(failedSubtests(long)[0]!.error.length, 5000);
  });
});

// The pairs themselves. Several selftest processes run at once under `node --test` (the selftest, both runners of the test above and
// the fault selftest's nested run: four in one `task web-e2e-ports`, each listening for ~10 s), and each one draws its own pair. A pair
// that was free when it was drawn is not a pair nobody else is about to use: the other process has drawn but not yet bound, so the
// "free now" check passes for both, and one of them finds the other's listener on a port it derives (CI run 42372437266: `failsWhileHeld`'s
// control saw `listening [[::1]:24638]` before it had listened on anything, #495). Two draws that share ANY derived port, whether or not
// they are the same pair, must not both be handed out.
describe("private pairs of concurrent selftests share no port", () => {
  const portsOf = (p: Pair) => derivePorts({ E2E_RPC_PORT: String(p.rpc), E2E_WEB_PORT: String(p.web) }).all;
  const shared = (a: Pair, b: Pair) => portsOf(a).filter((port) => portsOf(b).includes(port));
  // The pairs these cases force are drawn below the OS's ephemeral range (macOS: 49152 and up): a port in it can be taken by
  // someone's outgoing connection for a moment, the free-now check then refuses a draw the case needs accepted, and the case
  // would fail for a reason that is not the claim. (The selftest's own draw is random over the whole range and simply draws again.)
  const calm = (): Pair => ({ rpc: randomInt(20_000, 30_000), web: randomInt(40_000, 48_500) });
  const farWeb = (p: Pair) => (p.web >= 44_500 ? p.web - 4_000 : p.web + 4_000);
  /** A draw that shares ports with `p` without being `p`: its rpc is p's role rpc (+10), its web is far away. */
  const overlapping = (p: Pair): Pair => ({ rpc: p.rpc + 10, web: farWeb(p) });
  const dirs: string[] = [];
  const claimDir = () => {
    const d = mkdtempSync(join(tmpdir(), "assert-clean-claims-"));
    dirs.push(d);
    return d;
  };
  after(() => dirs.forEach((d) => rmSync(d, { recursive: true, force: true })));
  const offering = (first: Pair[]) => {
    const queue = [...first];
    let offered = 0;
    return { draw: () => (offered++, queue.shift() ?? randomPair()), offered: () => offered };
  };

  it("in one process: a draw that repeats a pair held by an earlier caller, and one that overlaps it, are both refused", async () => {
    const dir = claimDir();
    const held = await privatePair({ claimDir: dir, draw: calm });
    const forced = offering([held, overlapping(held)]);
    const second = await privatePair({ claimDir: dir, draw: forced.draw });
    assert.deepEqual(shared(held, second), [], `the second caller was handed ${JSON.stringify({ rpc: second.rpc, web: second.web })}, which shares ports with ${JSON.stringify({ rpc: held.rpc, web: held.web })}`);
    assert.ok(forced.offered() >= 3, `control: both forced draws must have been offered and refused, then a free one taken (offered ${forced.offered()})`);
  });

  it("in another process: the same two draws are refused while this process holds the pair", async () => {
    const dir = claimDir();
    const held = await privatePair({ claimDir: dir, draw: calm });
    const r = await runNested(["--experimental-strip-types", "e2e/fault/pair-child.ts", dir, `${held.rpc},${held.web}`, `${overlapping(held).rpc},${overlapping(held).web}`], { cwd: consoleDir, env: process.env, deadlineMs: 60_000 });
    assert.equal(r.code, 0, r.out);
    const got = JSON.parse(r.out.trim().split("\n").at(-1)!) as Pair & { draws: number };
    assert.deepEqual(shared(held, got), [], `the other process was handed ${JSON.stringify({ rpc: got.rpc, web: got.web })}, which shares ports with ${JSON.stringify({ rpc: held.rpc, web: held.web })}`);
    assert.ok(got.draws >= 3, `control: the child must have been offered both forced draws (it drew ${got.draws} times)`);
  });

  it("a released pair can be drawn again", async () => {
    const dir = claimDir();
    const held = await privatePair({ claimDir: dir, draw: calm });
    held.release();
    const again = await privatePair({ claimDir: dir, draw: offering([held]).draw });
    assert.deepEqual({ rpc: again.rpc, web: again.web }, { rpc: held.rpc, web: held.web }, "release() did not give the pair back");
  });

  it("a process that ends gives its pair back without being asked", async () => {
    const dir = claimDir();
    const probe = await privatePair({ claimDir: dir, draw: calm });
    probe.release();
    const r = await runNested(["--experimental-strip-types", "e2e/fault/pair-child.ts", dir, `${probe.rpc},${probe.web}`], { cwd: consoleDir, env: process.env, deadlineMs: 60_000 });
    assert.equal(r.code, 0, r.out);
    const got = JSON.parse(r.out.trim().split("\n").at(-1)!) as Pair & { draws: number };
    assert.deepEqual({ rpc: got.rpc, web: got.web }, { rpc: probe.rpc, web: probe.web }, "control: the child must have taken the pair it was offered, or this proves nothing");
    const again = await privatePair({ claimDir: dir, draw: offering([probe]).draw });
    assert.deepEqual({ rpc: again.rpc, web: again.web }, { rpc: probe.rpc, web: probe.web }, "the child ended and its pair is still taken");
  });

  it("a draw refused for sharing a port gives back the ports it had claimed before it hit the shared one", async () => {
    const dir = claimDir();
    const held = await privatePair({ claimDir: dir, draw: calm });
    // rpc one below held's: its first port (rpc - 1) is free and is claimed, its second (= held's rpc) is held and refuses it
    const partial: Pair = { rpc: held.rpc - 1, web: farWeb(held) };
    const second = await privatePair({ claimDir: dir, draw: offering([partial]).draw });
    assert.deepEqual(shared(held, second), [], "control: the partial overlap was refused");
    held.release();
    const third = await privatePair({ claimDir: dir, draw: offering([partial]).draw });
    assert.deepEqual({ rpc: third.rpc, web: third.web }, { rpc: partial.rpc, web: partial.web }, "the refused draw kept some of its ports claimed");
  });

  it("a draw refused because one of its ports is bound gives back every claim it made", async () => {
    const dir = claimDir();
    const d = await privatePair({ claimDir: dir, draw: calm });
    d.release();
    const busy = createServer();
    await new Promise<void>((resolve, reject) => (busy.once("error", reject), busy.listen({ port: portsOf(d)[5]!, host: "127.0.0.1" }, resolve)));
    try {
      const forced = offering([d]);
      const other = await privatePair({ claimDir: dir, draw: forced.draw });
      assert.deepEqual(shared(d, other), [], "control: a draw with a bound port was handed out");
      assert.ok(forced.offered() >= 2, "control: the draw with the bound port was offered");
    } finally {
      await new Promise<void>((resolve) => busy.close(() => resolve()));
    }
    const again = await privatePair({ claimDir: dir, draw: offering([d]).draw });
    assert.deepEqual({ rpc: again.rpc, web: again.web }, { rpc: d.rpc, web: d.web }, "the refused draw kept its ports claimed");
  });

  // A tripwire, not a barrier: the selftest takes its pair from e2e/private-pair.ts and draws none of its own.
  it("assert-clean.selftest.ts takes its pair from private-pair.ts, and holds the claim until it ends", () => {
    const source = readFileSync(fileURLToPath(new URL("./assert-clean.selftest.ts", import.meta.url)), "utf8");
    assert.match(source, /import \{ privatePair \} from "\.\/private-pair\.ts";/);
    assert.ok(!source.includes("randomInt"), "the selftest draws a pair of its own again");
    assert.match(source, /\.release\(\)/, "the selftest never releases its pair");
  });
});
