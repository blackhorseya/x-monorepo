import assert from "node:assert/strict";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";
import { judge, scan, stripComments, type Exception } from "./testing/retryScan.ts";

//   node --test --experimental-strip-types --import ./src/testing/register.mjs src/retry-overrides.test.ts   (task web-test)
//
// Static guard behind src/queries.retry.test.ts: no file of the console sets `retry` / `retryDelay` (or changes
// the QueryClient defaults) except queryClient.ts. The matrix proves what the policy does to the queries that
// exist; this catches the override itself, also in a query the matrix cannot reach (an inline useQuery in a
// route, a mutation, a helper).

const consoleRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const HOME = "src/queryClient.ts";

// A legitimate override: the file, a fragment of the offending line, why it differs from the shared policy,
// and the test (path under web/apps/console) that pins its behavior. None today: every query follows the one
// policy. Adding one is a review decision, which is the point of the list.
const EXCEPTIONS: Exception[] = [];

function sources(dir: string): { path: string; text: string }[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((d) => {
    const p = join(dir, d.name);
    if (d.isDirectory()) return sources(p);
    if (!/\.tsx?$/.test(d.name) || d.name.endsWith(".test.ts")) return [];
    return [{ path: relative(consoleRoot, p), text: readFileSync(p, "utf8") }];
  });
}

describe("no retry policy outside queryClient.ts", () => {
  const files = sources(join(consoleRoot, "src"));
  it("scans the source tree (not an empty directory, and it does see queryClient.ts and queries.ts)", () => {
    assert.ok(files.length > 20, `${files.length} files`);
    assert.ok(files.some((f) => f.path === HOME));
    assert.ok(files.some((f) => f.path === "src/queries.ts"));
    assert.ok(!files.some((f) => f.path.endsWith(".test.ts")));
  });
  it("queryClient.ts is where the policy is set (the scan finds it there)", () => {
    const home = scan(files).filter((h) => h.file === HOME);
    assert.ok(home.some((h) => /retry:\s*shouldRetry/.test(h.text)), "queries.retry");
    assert.ok(home.some((h) => /retry:\s*0/.test(h.text)), "mutations.retry");
  });
  it("every other hit is a listed exception with a reason and a test", () => {
    const { problems } = judge(scan(files), EXCEPTIONS, HOME, (t) => existsSync(join(consoleRoot, t)));
    assert.deepEqual(problems, []);
  });
});

describe("the scanner", () => {
  const f = (text: string, path = "src/x.ts") => [{ path, text }];
  it("finds the property forms", () => {
    for (const text of [
      "queryOptions({ retry: false })",
      "queryOptions({ retry: 3,\n retryDelay: 100 })",
      'queryOptions({ "retry": 3 })',
      "const retry = 1; queryOptions({ retry })",
      "queryOptions({ retry, staleTime: 1 })",
      "opts.retry = 3",
      "useQuery({ ...q, retryDelay: () => 1 })",
    ]) {
      assert.ok(scan(f(text)).length >= 1, text);
    }
  });
  it("finds a client built or re-defaulted elsewhere", () => {
    assert.equal(scan(f("const c = new QueryClient({})")).length, 1);
    assert.equal(scan(f("qc.setDefaultOptions({})")).length, 1);
    assert.equal(scan(f("qc.setQueryDefaults(key, {})")).length, 1);
    assert.equal(scan(f("qc.setMutationDefaults(key, {})")).length, 1);
  });
  it("gives the real line number", () => {
    assert.deepEqual(scan(f("a\n\nqueryOptions({ retry: 3 })")).map((h) => h.line), [3]);
  });
  it("ignores comments, reads and words that only contain retry", () => {
    for (const text of [
      "// retry: 3",
      "/* retry: 3 */",
      "/* a\n retry: 3\n b */ const x = 1",
      "const n = q.options.retry",
      "const retryOnMount = 1",
      "queryOptions({ retryOnMount: false })",
      "if (opts.retry === 3) {}",
      "qc.invalidateQueries()",
    ]) {
      assert.deepEqual(scan(f(text)), [], text);
    }
  });
  it("a // inside a string does not hide what follows on the line", () => {
    assert.equal(scan(f('fetch("http://x"); queryOptions({ retry: 3 })')).length, 1);
    assert.equal(stripComments('"a//b" // c'), '"a//b"     ');
  });
  it("keeps line numbers through a block comment", () => {
    assert.equal(stripComments("a /* x\ny */ b").split("\n").length, 2);
  });
  it("judge: a hit outside the home file is a problem, inside it is not", () => {
    const hit = scan(f("queryOptions({ retry: 3 })", "src/queries.ts"));
    assert.equal(judge(hit, [], "src/queryClient.ts", () => true).problems.length, 1);
    assert.deepEqual(judge(scan(f("{ retry: 0 }", "src/queryClient.ts")), [], "src/queryClient.ts", () => true).problems, []);
  });
  it("judge: an exception needs a reason and an existing test, and must still match", () => {
    const hit = scan(f("queryOptions({ retry: 3 })", "src/queries.ts"));
    const ok: Exception = { file: "src/queries.ts", match: "retry: 3", reason: "why", test: "t.test.ts" };
    const home = "src/queryClient.ts";
    assert.deepEqual(judge(hit, [ok], home, () => true).problems, []);
    assert.equal(judge(hit, [{ ...ok, reason: " " }], home, () => true).problems.length, 1);
    assert.equal(judge(hit, [{ ...ok, test: "" }], home, () => true).problems.length, 1);
    assert.equal(judge(hit, [ok], home, () => false).problems.length, 1);
    assert.equal(judge([], [ok], home, () => true).problems.length, 1);
  });
});
