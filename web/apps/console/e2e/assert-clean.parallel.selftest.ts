import { type ChildProcess, spawn } from "node:child_process";
import { describe, it, after } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";

// Two selftest runs at the same time, each under its own caller E2E pair (the way reviewers and parallel
// worktrees run), must both pass: the selftest brings the ports it needs and takes nothing the caller's
// pair or the other run holds (#386). A selftest with ports of its own written in would collide here.
const consoleDir = fileURLToPath(new URL("..", import.meta.url));
const children: ChildProcess[] = [];
const killGroup = (c: ChildProcess) => {
  try {
    if (c.pid) process.kill(-c.pid, "SIGKILL");
  } catch {
    // already gone
  }
};
after(() => children.forEach(killGroup));

function runner(pair: { rpc: number; web: number }): Promise<{ code: number | null; out: string }> {
  return new Promise((resolve, reject) => {
    // Inside `node --test`, NODE_TEST_CONTEXT makes a nested `node --test` run quietly in-process and exit 0
    // without running anything: a passing control that tested nothing. Strip it, and check below that the
    // runner really ran its tests. The reporter is named, not left to the TTY check: spec on a terminal,
    // TAP in CI (#386 — the first version matched the spec summary and failed on GitHub).
    const { NODE_TEST_CONTEXT: _context, ...parentEnv } = process.env;
    const child = spawn("node", ["--test", "--test-reporter=tap", "--experimental-strip-types", "e2e/assert-clean.selftest.ts"], {
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

describe("assert-clean selftest, run in parallel", () => {
  it("two runners with different E2E pairs both exit 0", async () => {
    const [a, b] = await Promise.all([runner({ rpc: 33131, web: 44131 }), runner({ rpc: 34131, web: 45131 })]);
    for (const [name, r] of [["A", a], ["B", b]] as const) assert.match(r.out, /^# pass [1-9]/m, `runner ${name} ran no tests (an empty run is not a pass):\n${r.out.slice(-800)}`);
    assert.equal(a.code, 0, `runner A (33131/44131) failed:\n${a.out.slice(-1500)}`);
    assert.equal(b.code, 0, `runner B (34131/45131) failed:\n${b.out.slice(-1500)}`);
  });
});
