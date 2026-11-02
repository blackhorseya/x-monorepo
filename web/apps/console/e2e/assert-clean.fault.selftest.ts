import { spawn } from "node:child_process";
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";

// The selftest must not turn a failure to bind into a pass. If ::1 is taken after the selftest chose its
// ports and before its IPv6 case listens, the case has to FAIL with the reason (EADDRINUSE) — not be
// skipped as "no IPv6 here" (#386). Only a host that has no ::1 at all (EADDRNOTAVAIL, EAFNOSUPPORT,
// EPROTONOSUPPORT) may skip. e2e/fault/steal-ipv6.mjs injects the take-over into the selftest process.
const consoleDir = fileURLToPath(new URL("..", import.meta.url));

function selftest(args: string[]): Promise<{ code: number | null; out: string }> {
  return new Promise((resolve, reject) => {
    // NODE_TEST_CONTEXT would make this nested runner exit 0 without running anything (see the parallel selftest).
    const { NODE_TEST_CONTEXT: _context, ...env } = process.env;
    const child = spawn("node", [...args, "--test", "--test-reporter=tap", "--experimental-strip-types", "e2e/assert-clean.selftest.ts"], { cwd: consoleDir, env, stdio: ["ignore", "pipe", "pipe"] });
    let out = "";
    child.stdout!.on("data", (d) => (out += d));
    child.stderr!.on("data", (d) => (out += d));
    child.once("error", reject);
    child.once("exit", (code) => resolve({ code, out }));
  });
}

describe("assert-clean selftest does not hide a failed bind", () => {
  it("control: without the injection it passes, with no skipped case", async () => {
    const r = await selftest([]);
    assert.equal(r.code, 0, r.out.slice(-1500));
    assert.match(r.out, /^# pass [1-9]/m);
    assert.match(r.out, /^# skipped 0/m, "the control must run the IPv6 case (this host has ::1)");
  });

  it("::1 taken after the ports were chosen: non-zero exit, and the reason is EADDRINUSE", async () => {
    const r = await selftest(["--import", fileURLToPath(new URL("./fault/steal-ipv6.mjs", import.meta.url))]);
    assert.notEqual(r.code, 0, `the selftest passed although ::1 was taken:\n${r.out.slice(-1500)}`);
    assert.match(r.out, /EADDRINUSE/, `it failed, but not for the injected reason:\n${r.out.slice(-1500)}`);
  });
});
