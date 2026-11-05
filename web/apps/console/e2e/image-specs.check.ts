import { execFileSync } from "node:child_process";
import { basename } from "node:path";
import { checkSpecClasses, imageSpecs, previewOnlySpecs, type Listed } from "./image-specs.ts";

// The gate for #449: `playwright test --list` (no server starts) says what the "image" and "chromium" projects
// would run, and that has to match e2e/image-specs.ts and the set pinned below. A spec nobody classified, one in
// both lists, a pinned spec that is missing and an image spec that is not pinned all exit 1.
//
//   node --experimental-strip-types e2e/image-specs.check.ts      (task web-e2e-image runs it first;
//   e2e/image-specs.selftest.ts proves it goes red on each of those)
//
// PINNED is NOT derived from image-specs.ts: if it were, adding a spec to the image list would move the
// expectation with it and nothing would notice.
const PINNED = ["image.spec.ts", "version.spec.ts", "orders.spec.ts", "order.spec.ts", "products.spec.ts", "reservations.spec.ts"];

interface JSONReport extends JSONSuite {
  errors?: unknown[];
}

interface JSONSuite {
  specs?: { file: string; tests: { projectName: string }[] }[];
  suites?: JSONSuite[];
}

export function parseListing(json: string): Listed {
  const root = JSON.parse(json) as JSONReport;
  // a spec that does not load is reported here and may not change the exit code: a listing with errors is not a listing
  if ((root.errors ?? []).length > 0) throw new Error(`playwright --list reported ${root.errors!.length} error(s): ${JSON.stringify(root.errors).slice(0, 400)}`);
  const files: Record<string, Set<string>> = { chromium: new Set(), image: new Set() };
  const walk = (s: JSONSuite) => {
    for (const sp of s.specs ?? []) {
      // top-level only: the chromium project's testIgnore already keeps out the subdirectories, a nested file never counts
      if (sp.file.includes("/")) continue;
      for (const t of sp.tests) files[t.projectName]?.add(basename(sp.file));
    }
    for (const c of s.suites ?? []) walk(c);
  };
  walk(root);
  return { chromium: [...files.chromium].sort(), image: [...files.image].sort() };
}

if (import.meta.url === `file://${process.argv[1]}`) {
  // E2E_CONSOLE_URL set so the listing is what a console-image run lists; E2E_DEX off: the dex project is not under test.
  const env: NodeJS.ProcessEnv = { ...process.env, E2E_CONSOLE_URL: "http://127.0.0.1:1" };
  delete env.E2E_DEX;
  const out = execFileSync("pnpm", ["exec", "playwright", "test", "--list", "--reporter=json"], { env, encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
  const listed = parseListing(out);
  if (listed.chromium.length === 0 || listed.image.length === 0) {
    console.error(`image-specs: the listing is empty (chromium=${listed.chromium.length}, image=${listed.image.length}), refusing to pass on nothing`);
    process.exit(1);
  }
  const problems = checkSpecClasses(listed, { image: imageSpecs, previewOnly: previewOnlySpecs }, PINNED);
  if (problems.length) {
    console.error("image-specs: the image / preview split is broken:\n  " + problems.join("\n  "));
    process.exit(1);
  }
  console.log(`image-specs: ok, image runs ${listed.image.length} spec files (${listed.image.join(", ")}), ${listed.chromium.length - listed.image.length} stay on vite preview`);
}
