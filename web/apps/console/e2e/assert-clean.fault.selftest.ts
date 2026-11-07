import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { killNested, runNested } from "./nested.ts";
import { explainFailure } from "./tap.ts";

// The selftest must not turn a failure to bind into a pass. If ::1 is taken after the selftest chose its
// ports and before its IPv6 case listens, the case has to FAIL with the reason (EADDRINUSE) — not be
// skipped as "no IPv6 here" (#386). Only a host that has no ::1 at all (EADDRNOTAVAIL, EAFNOSUPPORT,
// EPROTONOSUPPORT) may skip. e2e/fault/steal-ipv6.mjs injects the take-over into the selftest process.
const consoleDir = fileURLToPath(new URL("..", import.meta.url));

const FAULT_DEADLINE_MS = 90_000;
const TEST_TIMEOUT_MS = 150_000;
after(killNested);
const selftest = (args: string[]) =>
  // NODE_TEST_CONTEXT would make this nested runner exit 0 without running anything (see the parallel selftest): runNested strips it.
  runNested([...args, "--test", "--test-reporter=tap", "--experimental-strip-types", "e2e/assert-clean.selftest.ts"], { cwd: consoleDir, env: process.env, deadlineMs: FAULT_DEADLINE_MS });

describe("assert-clean selftest does not hide a failed bind", () => {
  it("control: without the injection it passes, with no skipped case", { timeout: TEST_TIMEOUT_MS }, async () => {
    const r = await selftest([]);
    assert.equal(r.code, 0, explainFailure(r));
    assert.match(r.out, /^# pass [1-9]/m);
    assert.match(r.out, /^# skipped 0/m, "the control must run the IPv6 case (this host has ::1)");
  });

  it("::1 taken after the ports were chosen: non-zero exit, and the reason is EADDRINUSE", { timeout: TEST_TIMEOUT_MS }, async () => {
    const r = await selftest(["--import", fileURLToPath(new URL("./fault/steal-ipv6.mjs", import.meta.url))]);
    assert.notEqual(r.code, 0, `the selftest passed although ::1 was taken:\n${explainFailure(r)}`);
    assert.match(r.out, /EADDRINUSE/, `it failed, but not for the injected reason:\n${explainFailure(r)}`);
  });
});
