import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, it } from "node:test";
import { checkSpecClasses, imageSpecs, previewOnlySpecs } from "./image-specs.ts";

// The gate (e2e/image-specs.check.ts) must go red on each way the split can rot, and stay green on the real one:
// a gate that cannot fail is worth nothing (and "green because both sides are empty" is how that happens).
//   node --test --experimental-strip-types e2e/image-specs.selftest.ts      (task web-e2e-image runs it first)
const PINNED = ["image.spec.ts", "version.spec.ts", "admin.spec.ts"];
const all = [...imageSpecs, ...previewOnlySpecs].sort();
const lists = { image: imageSpecs, previewOnly: previewOnlySpecs };
const listed = { chromium: all, image: [...imageSpecs] };

describe("image-specs gate", () => {
  it("the real split is sound", () => {
    assert.deepEqual(checkSpecClasses(listed, lists, PINNED), []);
  });
  it("a new spec nobody classified is red", () => {
    const p = checkSpecClasses({ ...listed, chromium: [...all, "refund.spec.ts"] }, lists, PINNED);
    assert.deepEqual(p.map((x) => x.split(":")[0]), ["refund.spec.ts"]);
  });
  it("a spec in both lists is red", () => {
    const p = checkSpecClasses(listed, { image: imageSpecs, previewOnly: [...previewOnlySpecs, "admin.spec.ts"] }, PINNED);
    assert.ok(p.some((x) => x.startsWith("admin.spec.ts: in both")), p.join("\n"));
  });
  it("one more spec marked for the image (a preview-only one moved over) is red", () => {
    const moved = previewOnlySpecs[0];
    const p = checkSpecClasses(
      { chromium: all, image: [...imageSpecs, moved] },
      { image: [...imageSpecs, moved], previewOnly: previewOnlySpecs.filter((f) => f !== moved) },
      PINNED
    );
    assert.deepEqual(p.map((x) => x.split(":")[0]), [moved]);
  });
  it("one image spec left out of the image project is red", () => {
    const p = checkSpecClasses(
      { chromium: all, image: imageSpecs.filter((f) => f !== "admin.spec.ts") },
      { image: imageSpecs.filter((f) => f !== "admin.spec.ts"), previewOnly: previewOnlySpecs },
      PINNED
    );
    assert.ok(p.some((x) => x.startsWith("admin.spec.ts")), p.join("\n"));
  });
  it("a listed file that no longer exists is red", () => {
    const p = checkSpecClasses({ ...listed, chromium: all.filter((f) => f !== "credential-switch.spec.ts") }, lists, PINNED);
    assert.ok(p.some((x) => x.startsWith("credential-switch.spec.ts: listed")), p.join("\n"));
  });
  it("a project that runs more than the list says is red", () => {
    const p = checkSpecClasses({ chromium: all, image: [...imageSpecs, "credential-switch.spec.ts"] }, lists, PINNED);
    assert.ok(p.some((x) => x.startsWith("credential-switch.spec.ts: runs on the image")), p.join("\n"));
  });
});

// The external command the gate runs is `pnpm exec playwright test --list --reporter=json`. A fake pnpm first on PATH answers
// with the given stdout and exit code, so the gate's reaction to a failing / partial / empty / erroring listing is observed
// on the real script: none of them may exit 0 (a gate that swallows the exit code of what it calls reports green on nothing).
const listing = (opts: { errors?: unknown[]; chromium?: string[]; image?: string[] } = {}) =>
  JSON.stringify({
    errors: opts.errors ?? [],
    suites: [
      {
        specs: [
          ...(opts.chromium ?? all).map((f) => ({ file: f, tests: [{ projectName: "chromium" }] })),
          ...(opts.image ?? imageSpecs).map((f) => ({ file: f, tests: [{ projectName: "image" }] })),
        ],
      },
    ],
  });

function runGate(stdout: string, exit: number): { status: number | null; stderr: string; stdout: string } {
  const dir = mkdtempSync(join(tmpdir(), "image-specs-fake-"));
  try {
    writeFileSync(join(dir, "out.json"), stdout);
    writeFileSync(join(dir, "pnpm"), `#!/bin/sh\ncat "${join(dir, "out.json")}"\nexit ${exit}\n`);
    chmodSync(join(dir, "pnpm"), 0o755);
    const check = fileURLToPath(new URL("./image-specs.check.ts", import.meta.url));
    const r = spawnSync(process.execPath, ["--experimental-strip-types", check], {
      env: { ...process.env, PATH: `${dir}:${process.env.PATH}` },
      encoding: "utf8",
    });
    return { status: r.status, stderr: r.stderr, stdout: r.stdout };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

describe("image-specs gate: the external command", () => {
  it("control: a sound listing, exit 0, passes", () => {
    const r = runGate(listing(), 0);
    assert.equal(r.status, 0, r.stderr);
  });
  it("a complete, sound listing but a non-zero exit is red", () => {
    assert.notEqual(runGate(listing(), 1).status, 0);
  });
  it("a truncated listing is red, with exit 1 and with exit 0", () => {
    const cut = listing().slice(0, 120);
    assert.notEqual(runGate(cut, 1).status, 0);
    assert.notEqual(runGate(cut, 0).status, 0);
  });
  it("an empty listing is red", () => {
    assert.notEqual(runGate("", 0).status, 0);
    assert.notEqual(runGate(listing({ chromium: [], image: [] }), 0).status, 0);
  });
  it("a listing that reports errors (a spec that does not load) is red even with exit 0", () => {
    assert.notEqual(runGate(listing({ errors: [{ message: "SyntaxError in e2e/admin.spec.ts" }] }), 0).status, 0);
  });
});
