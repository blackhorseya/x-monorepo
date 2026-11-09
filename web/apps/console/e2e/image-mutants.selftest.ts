import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { chmodSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, it } from "node:test";

// image-mutants.sh decides "this mutant went red on a spec" from a Playwright log and an exit code (e2e/image-mutants.lib.sh).
// It calls the verdict as `classify ... || c=$?`, a form in which bash switches errexit OFF inside the function: a stage of the
// parsing pipeline that fails after printing something would be ignored, and a half-parsed log reported as RED (#460 r1).
// The verdict is tested on its own (fakes for each stage on PATH that print a plausible line and exit 42) and through the real
// script (image-mutants.sh, which `task web-e2e-image-mutants` runs) with a fake image.sh (E2E_IMAGE_SH): in both a failure of
// the harness has to read ERROR (2), never RED (0).
//   node --test --experimental-strip-types e2e/image-mutants.selftest.ts      (task web-e2e-image-mutants runs it first)
const here = fileURLToPath(new URL(".", import.meta.url));
const lib = join(here, "image-mutants.lib.sh");
const script = join(here, "image-mutants.sh");
const consoleDir = join(here, "..");

const FAIL_LINE = "  ✘   1 [image] › e2e/image.spec.ts:12:1 › deep link (34ms)";
const OK_LINE = "  ✓   2 [image] › e2e/account-loop.spec.ts:12:1 › lands (500ms)";
const SUMMARY_RED = ["  1 failed", "  19 passed (12.5s)"];
const SUMMARY_GREEN = ["  20 passed (12.5s)"];
const realOf = (cmd: string) => execFileSync("sh", ["-c", `command -v ${cmd}`], { encoding: "utf8" }).trim();

function sandbox(): string {
  const d = mkdtempSync(join(tmpdir(), "image-mutants-selftest-"));
  mkdirSync(join(d, "fake"));
  return d;
}
function writeExec(path: string, body: string) {
  writeFileSync(path, body);
  chmodSync(path, 0o755);
}
/** a fake `cmd` on PATH: prints `out`, then exits 42; only when `when` (a sh `case` pattern over "$*") matches, else the real one runs */
function fake(d: string, cmd: string, out: string, when = "*") {
  writeExec(
    join(d, "fake", cmd),
    `#!/bin/sh\ncase "$*" in\n${when}) printf '%s\\n' '${out}'; exit 42;;\nesac\nexec ${realOf(cmd)} "$@"\n`
  );
}
// IMAGE_MUTANTS_CELL: set by whoever injects fakes into `task web-e2e-image-mutants` from outside (the fault-injection cells in the PR
// comment); this selftest is that task's first step and must not inherit them, its children get their own fakes only.
const baseEnv = (): NodeJS.ProcessEnv => {
  const e = { ...process.env };
  delete e.IMAGE_MUTANTS_CELL;
  return e;
};
const withFakes = (d: string) => ({ ...baseEnv(), PATH: `${join(d, "fake")}:${process.env.PATH}` });

/** the verdict, called the way image-mutants.sh calls it */
function verdict(d: string, logLines: string[], code: number): { out: string; status: number } {
  const log = join(d, "run.log");
  writeFileSync(log, logLines.join("\n") + "\n");
  const r = spawnSync("bash", ["-c", `set -euo pipefail; source "${lib}"; c=0; classify m "${log}" ${code} || c=$?; echo "status=$c"`], { env: withFakes(d), encoding: "utf8" });
  const m = /status=(\d+)/.exec(r.stdout);
  assert.ok(m, `no status line: ${r.stdout} ${r.stderr}`);
  return { out: r.stdout, status: Number(m[1]) };
}

/** the real script, one mutant, with a fake image.sh that prints `log` and exits `code` */
function runScript(d: string, log: string[], code: number, env: NodeJS.ProcessEnv = withFakes(d)) {
  const imageSh = join(d, "image.sh");
  writeFileSync(join(d, "image.log"), log.join("\n") + "\n");
  writeExec(imageSh, `#!/bin/sh\ncat "${join(d, "image.log")}"\nexit ${code}\n`);
  const r = spawnSync("bash", [script, "fallback"], { cwd: consoleDir, env: { ...env, E2E_IMAGE_SH: imageSh }, encoding: "utf8" });
  return { status: r.status, out: r.stdout + r.stderr };
}

// a stage that prints something and then fails
const STAGES = [
  ["sed (strip colours)", "sed", FAIL_LINE, "*x1b*"],
  ["grep", "grep", FAIL_LINE, "*"],
  ["sed (spec file)", "sed", "image.spec.ts", "*e2e*"],
  ["sort", "sort", "image.spec.ts", "*"],
  ["uniq", "uniq", "      1 image.spec.ts", "*"],
  ["awk", "awk", "image.spec.ts×1", "*"],
] as const;

describe("image-mutants verdict (classify)", () => {
  it("red: image.sh failed, a spec failed, Playwright finished", () => {
    const r = verdict(sandbox(), [OK_LINE, FAIL_LINE, FAIL_LINE, ...SUMMARY_RED], 1);
    assert.equal(r.status, 0);
    assert.match(r.out, /failed-spec-files=\[image\.spec\.ts×2\]/);
  });
  it("green: image.sh passed and Playwright finished", () => {
    assert.equal(verdict(sandbox(), [OK_LINE, ...SUMMARY_GREEN], 0).status, 1);
  });
  it("harness broke: image.sh failed before any spec ran", () => {
    assert.equal(verdict(sandbox(), ["image.sh: console container x not healthy"], 1).status, 2);
  });
  it("harness broke: a run cut off after some failed specs (no Playwright summary) is not a RED", () => {
    assert.equal(verdict(sandbox(), [OK_LINE, FAIL_LINE], 1).status, 2);
  });
  it("harness broke: exit 0 without a Playwright summary is not a GREEN", () => {
    assert.equal(verdict(sandbox(), ["image.sh: ok"], 0).status, 2);
  });
  it("harness broke: exit != 0 with a summary but no failed spec line", () => {
    assert.equal(verdict(sandbox(), [OK_LINE, ...SUMMARY_GREEN], 1).status, 2);
  });
  for (const [label, cmd, out, when] of STAGES) {
    it(`${label}: ${cmd} fails after printing, so the harness broke and it is not a RED`, () => {
      const d = sandbox();
      fake(d, cmd, out, when);
      const r = verdict(d, [OK_LINE, FAIL_LINE, ...SUMMARY_RED], 1);
      assert.equal(r.status, 2, r.out);
    });
  }
  it("the evidence copy that cannot be written is a failure", () => {
    const d = sandbox();
    writeFileSync(join(d, "src.log"), "x");
    const r = spawnSync("bash", ["-c", `set -euo pipefail; source "${lib}"; c=0; keep_log "${join(d, "src.log")}" "${join(d, "no-such-dir")}" m || c=$?; echo "status=$c"`], { encoding: "utf8" });
    assert.notEqual(/status=(\d+)/.exec(r.stdout)?.[1], "0");
  });
});

describe("image-mutants.sh as task web-e2e-image-mutants runs it (fake image.sh, fakes on PATH)", () => {
  it("control of the harness: a failed spec and a finished run is RED (as required), exit 0", () => {
    const r = runScript(sandbox(), [OK_LINE, FAIL_LINE, ...SUMMARY_RED], 1);
    assert.equal(r.status, 0, r.out);
    assert.match(r.out, /mutant fallback: RED \(as required\)/);
  });
  it("image.sh that finds nothing wrong: GREEN is reported as a miss, exit 1", () => {
    const r = runScript(sandbox(), [OK_LINE, ...SUMMARY_GREEN], 0);
    assert.equal(r.status, 1, r.out);
    assert.match(r.out, /did not notice/);
  });
  it("image.sh that prints a failed spec and then exits non-zero without finishing: ERROR, never RED", () => {
    const r = runScript(sandbox(), [OK_LINE, FAIL_LINE], 42);
    assert.equal(r.status, 1, r.out);
    assert.match(r.out, /mutant fallback: ERROR \(harness\)/);
    assert.doesNotMatch(r.out, /RED \(as required\)/);
  });
  it("image.sh that fails before running specs: ERROR, never RED", () => {
    const r = runScript(sandbox(), ["image.sh: console container x not healthy"], 1);
    assert.equal(r.status, 1, r.out);
    assert.match(r.out, /ERROR \(harness\)/);
  });
  for (const [label, cmd, out, when] of STAGES) {
    it(`${label}: ${cmd} fails after printing: ERROR (harness), never RED`, () => {
      const d = sandbox();
      fake(d, cmd, out, when);
      const r = runScript(d, [OK_LINE, FAIL_LINE, ...SUMMARY_RED], 1);
      assert.equal(r.status, 1, r.out);
      assert.match(r.out, /mutant fallback: ERROR \(harness\)/);
      assert.doesNotMatch(r.out, /RED \(as required\)/);
    });
  }
  it("a log directory that cannot be written: ERROR (harness), never RED", () => {
    const d = sandbox();
    const r = runScript(d, [OK_LINE, FAIL_LINE, ...SUMMARY_RED], 1, { ...withFakes(d), E2E_MUTANT_LOGS: join(d, "no-such-dir") });
    assert.equal(r.status, 1, r.out);
    assert.match(r.out, /ERROR \(harness\)/);
    assert.doesNotMatch(r.out, /RED \(as required\)/);
  });
});
