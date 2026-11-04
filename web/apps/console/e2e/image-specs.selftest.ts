import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { checkSpecClasses, imageSpecs, previewOnlySpecs } from "./image-specs.ts";

// The gate (e2e/image-specs.check.ts) must go red on each way the split can rot, and stay green on the real one:
// a gate that cannot fail is worth nothing (and "green because both sides are empty" is how that happens).
//   node --test --experimental-strip-types e2e/image-specs.selftest.ts      (task web-e2e-image runs it first)
const PINNED = ["image.spec.ts", "version.spec.ts", "orders.spec.ts", "order.spec.ts", "products.spec.ts", "reservations.spec.ts"];
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
    const p = checkSpecClasses(listed, { image: imageSpecs, previewOnly: [...previewOnlySpecs, "orders.spec.ts"] }, PINNED);
    assert.ok(p.some((x) => x.startsWith("orders.spec.ts: in both")), p.join("\n"));
  });
  it("one more spec marked for the image (a preview-only one moved over) is red", () => {
    const moved = "history.spec.ts";
    const p = checkSpecClasses(
      { chromium: all, image: [...imageSpecs, moved] },
      { image: [...imageSpecs, moved], previewOnly: previewOnlySpecs.filter((f) => f !== moved) },
      PINNED
    );
    assert.deepEqual(p.map((x) => x.split(":")[0]), [moved]);
  });
  it("one image spec left out of the image project is red", () => {
    const p = checkSpecClasses(
      { chromium: all, image: imageSpecs.filter((f) => f !== "products.spec.ts") },
      { image: imageSpecs.filter((f) => f !== "products.spec.ts"), previewOnly: previewOnlySpecs },
      PINNED
    );
    assert.ok(p.some((x) => x.startsWith("products.spec.ts")), p.join("\n"));
  });
  it("a listed file that no longer exists is red", () => {
    const p = checkSpecClasses({ ...listed, chromium: all.filter((f) => f !== "stock.spec.ts") }, lists, PINNED);
    assert.ok(p.some((x) => x.startsWith("stock.spec.ts: listed")), p.join("\n"));
  });
  it("a project that runs more than the list says is red", () => {
    const p = checkSpecClasses({ chromium: all, image: [...imageSpecs, "stock.spec.ts"] }, lists, PINNED);
    assert.ok(p.some((x) => x.startsWith("stock.spec.ts: runs on the image")), p.join("\n"));
  });
});
