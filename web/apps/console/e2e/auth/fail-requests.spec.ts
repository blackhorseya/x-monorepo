import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { failRequests, holdRequests, scriptByKey, watchRequests } from "../fail-requests";
import { settle } from "./helpers";

// holdRequests is a small state machine: armed (nothing parked) -> parked (>= 1 call waiting) -> released.
// Every transition has a test below: armed -> released with nothing parked, parked -> released (the parked
// call finishes), and a call arriving after release (passes through, not counted).
//
// The helpers of e2e/fail-requests.ts, against a real page.route and a real fetch: the "server" is a
// stub registered first, so it answers last (Playwright runs the newest route handler first, and the
// helpers hand everything they do not take on with route.fallback). A spec that uses a helper as a
// positive control ("the fault was hit") is only as good as these.

const OTHER_KEY = "some-other-key";
const call = (page: Page, method: string, key?: string, body = "{}") =>
  page.evaluate(
    ([m, k, b]) =>
      fetch(`/api/test.v1.TestService/${m}`, { method: "POST", headers: { "Content-Type": "application/json", ...(k ? { "X-API-Key": k } : {}) }, body: b })
        .then((r) => r.status)
        .catch(() => "failed"),
    [method, key, body] as const,
  );
// status and parsed body of one call (a call is answered with JSON by the stub and by every Reply but { http })
const callFull = (page: Page, method: string, key?: string) =>
  page.evaluate(
    ([m, k]) =>
      fetch(`/api/test.v1.TestService/${m}`, { method: "POST", headers: { "Content-Type": "application/json", ...(k ? { "X-API-Key": k } : {}) }, body: "{}" }).then(
        async (r) => [r.status, await r.text()] as const,
      ),
    [method, key] as const,
  );
const RPC = (m: string) => `test.v1.TestService/${m}`;

test.beforeEach(async ({ page }) => {
  await page.goto("/api/readyz"); // an origin to fetch from, nothing of the console running
  await page.route("**/api/test.v1.TestService/*", (route) => route.fulfill({ status: 200, contentType: "application/json", body: "{}" }));
});

test("failRequests: key omitted matches every caller, a string only its key, null only anonymous calls", async ({ page }) => {
  const any = await failRequests(page, { method: "Any", times: 3 });
  expect([await call(page, "Any"), await call(page, "Any", OPS_KEY), await call(page, "Any", OTHER_KEY)]).toEqual([503, 503, 503]);
  expect([any.matched(), any.failed()]).toEqual([3, 3]);

  const one = await failRequests(page, { method: "One", key: OPS_KEY, times: 3 });
  expect([await call(page, "One"), await call(page, "One", ADMIN_KEY), await call(page, "One", OPS_KEY)]).toEqual([200, 200, 503]);
  expect([one.matched(), one.failed()]).toEqual([1, 1]);

  const anon = await failRequests(page, { method: "Anon", key: null, times: 3 });
  expect([await call(page, "Anon", OPS_KEY), await call(page, "Anon")]).toEqual([200, 503]);
  expect([anon.matched(), anon.failed()]).toEqual([1, 1]);
});

test("failRequests: only the exact method, only `times` calls, and a reset is a network failure", async ({ page }) => {
  const f = await failRequests(page, { method: "ListAccounts" });
  expect(await call(page, "ListAccountsExtra")).toBe(200); // another method, even with the same prefix
  expect(await call(page, "XListAccounts")).toBe(200); // ... or one that merely contains it
  expect(await call(page, "ListAccount")).toBe(200);
  expect(await call(page, "ListPoison")).toBe(200); // another RPC altogether
  expect(f.matched()).toBe(0);
  expect(await call(page, "ListAccounts")).toBe(503); // the first
  expect(await call(page, "ListAccounts")).toBe(200); // times defaults to 1
  expect([f.matched(), f.failed()]).toEqual([2, 1]);

  const r = await failRequests(page, { method: "Reset", mode: "reset" });
  expect(await call(page, "Reset")).toBe("failed");
  expect(await call(page, "Reset")).toBe(200);
  expect(r.failed()).toBe(1);
});

test("failRequests: the 503 is a Connect unavailable error", async ({ page }) => {
  await failRequests(page, { method: "Body" });
  const body = await page.evaluate(() => fetch("/api/test.v1.TestService/Body", { method: "POST", body: "{}" }).then(async (r) => [r.status, await r.json()]));
  expect(body).toEqual([503, { code: "unavailable", message: "injected by failRequests" }]);
});

test("holdRequests: a matching call is parked, counted, and goes on after release()", async ({ page }) => {
  const held = await holdRequests(page, { method: "Slow", key: null });
  let arrived = false;
  void held.arrived().then(() => (arrived = true));
  await call(page, "Slow", OPS_KEY); // another caller: not held, answered at once
  await call(page, "Other"); // another method: same
  await settle(page);
  expect([held.count(), arrived]).toEqual([0, false]);

  let status: unknown;
  const pending = call(page, "Slow").then((s) => (status = s));
  await held.arrived();
  expect(held.count()).toBe(1);
  await settle(page);
  expect(status).toBeUndefined(); // still parked: the server has not been asked
  held.release();
  await pending;
  expect(status).toBe(200);
});

test("holdRequests: release() before any call parks nothing, and counts nothing", async ({ page }) => {
  const held = await holdRequests(page, { method: "Late" });
  let arrived = false;
  void held.arrived().then(() => (arrived = true));
  held.release();
  expect(await call(page, "Late")).toBe(200); // goes straight through ...
  expect(await call(page, "Late", OPS_KEY)).toBe(200);
  await settle(page);
  expect([held.count(), arrived]).toEqual([0, false]); // ... and the positive control stays false
});

test("holdRequests: parked, then released, then a second call: the second is not counted", async ({ page }) => {
  const held = await holdRequests(page, { method: "Again" });
  let status: unknown;
  const first = call(page, "Again").then((s) => (status = s));
  await held.arrived();
  expect([held.count(), status]).toEqual([1, undefined]); // positive control: parked, the call has not finished
  held.release();
  await first;
  expect(status).toBe(200); // positive control: released, the call finished
  expect(await call(page, "Again")).toBe(200); // the second one never waits ...
  expect(await call(page, "Again", OPS_KEY)).toBe(200);
  expect(held.count()).toBe(1); // ... and is not counted as parked
});

// holdRequests' own key semantics and exact method, one row per target: which of an anonymous call, OPS_KEY's
// and another key's call get parked (and counted), and which go straight through (answered 200, count 0).
const HOLD_ROWS: { name: string; key: string | null | undefined; parked: [boolean, boolean, boolean] }[] = [
  { name: "omitted: every caller", key: undefined, parked: [true, true, true] },
  { name: "string: only that key", key: OPS_KEY, parked: [false, true, false] },
  { name: "null: only anonymous calls", key: null, parked: [true, false, false] },
];
for (const row of HOLD_ROWS) {
  test(`holdRequests: key ${row.name}`, async ({ page }) => {
    const held = await holdRequests(page, row.key === undefined ? { method: "Keyed" } : { method: "Keyed", key: row.key });
    const callers: (string | undefined)[] = [undefined, OPS_KEY, OTHER_KEY]; // anonymous, OPS_KEY, another key
    const pending = callers.map((k) => {
      const c = { status: undefined as unknown };
      void call(page, "Keyed", k).then((st) => (c.status = st));
      return c;
    });
    await expect.poll(() => held.count()).toBe(row.parked.filter(Boolean).length);
    await settle(page);
    // the ones that were not matched are answered already, the parked ones are still waiting
    expect(pending.map((c) => c.status !== undefined)).toEqual(row.parked.map((p) => !p));
    held.release();
    await expect.poll(() => pending.map((c) => c.status)).toEqual([200, 200, 200]);
    expect(held.count()).toBe(row.parked.filter(Boolean).length); // release() did not add to it
  });
}

test("holdRequests: only the exact method; other RPCs and same-prefix names are not parked", async ({ page }) => {
  const held = await holdRequests(page, { method: "Slow" });
  expect(await call(page, "SlowExtra")).toBe(200);
  expect(await call(page, "XSlow")).toBe(200);
  expect(await call(page, "Slo")).toBe(200);
  expect(await call(page, "ListAccounts")).toBe(200);
  await settle(page);
  expect(held.count()).toBe(0);
});

test("holdRequests: several calls are parked together, and the helpers compose", async ({ page }) => {
  const held = await holdRequests(page, { method: "Two" });
  const fault = await failRequests(page, { method: "Two", key: OPS_KEY }); // registered later: sees the call first
  const a = call(page, "Two"); // anonymous: parked by the hold
  const b = call(page, "Two", OPS_KEY); // the fault takes it before the hold sees it
  expect(await b).toBe(503);
  await held.arrived();
  const c = call(page, "Two", OTHER_KEY);
  await expect.poll(() => held.count()).toBe(2);
  held.release();
  expect([await a, await c]).toEqual([200, 200]);
  expect([fault.failed(), held.count()]).toEqual([1, 2]);
});

// ---- scriptByKey ---------------------------------------------------------------------------------------

test("scriptByKey: each key gets its own reply (ok, Connect error, plain http status)", async ({ page }) => {
  await scriptByKey(page, RPC("Plan"), { "": { connect: "unauthenticated" }, [OPS_KEY]: { ok: { who: "ops" } }, [ADMIN_KEY]: { http: 418 } });
  expect(await callFull(page, "Plan")).toEqual([401, JSON.stringify({ code: "unauthenticated", message: "injected: unauthenticated" })]);
  expect(await callFull(page, "Plan", OPS_KEY)).toEqual([200, JSON.stringify({ who: "ops" })]);
  expect(await callFull(page, "Plan", ADMIN_KEY)).toEqual([418, "injected"]);
});

test("scriptByKey: a key with no plan is answered 599, not passed on to the server", async ({ page }) => {
  await scriptByKey(page, RPC("NoPlan"), { [OPS_KEY]: { ok: {} } });
  expect(await callFull(page, "NoPlan", OTHER_KEY)).toEqual([599, `scriptByKey: no plan for key "${OTHER_KEY}"`]); // the stub behind it says 200
});

test("scriptByKey: count(key) is per key", async ({ page }) => {
  const script = await scriptByKey(page, RPC("Count"), { "": { ok: {} }, [OPS_KEY]: { ok: {} } });
  await call(page, "Count");
  await call(page, "Count", OPS_KEY);
  await call(page, "Count", OPS_KEY);
  expect([script.count(""), script.count(OPS_KEY), script.count(OTHER_KEY)]).toEqual([1, 2, 0]);
});

test("scriptByKey: a call refused with 599 has reached the script and counts", async ({ page }) => {
  const script = await scriptByKey(page, RPC("Refused"), { "": { ok: {} } });
  await call(page, "Refused", OTHER_KEY);
  expect([script.count(OTHER_KEY), script.count("")]).toEqual([1, 0]);
});

test("scriptByKey: only the exact rpc is scripted; another method, a same-prefix name and a same-suffix name pass on", async ({ page }) => {
  const script = await scriptByKey(page, RPC("Exact"), { "": { connect: "unavailable" } });
  expect(await call(page, "ExactExtra")).toBe(200);
  expect(await call(page, "XExact")).toBe(200);
  expect(await call(page, "ListAccounts")).toBe(200);
  expect(script.count("")).toBe(0);
  expect(await call(page, "Exact")).toBe(503);
  expect(script.count("")).toBe(1);
});

test("scriptByKey + holdRequests: a parked call has not reached the script; after release() the script answers it (hold registered after the script)", async ({ page }) => {
  const script = await scriptByKey(page, RPC("Held"), { "": { ok: { from: "script" } } });
  const held = await holdRequests(page, { method: "Held", key: null });
  let got: unknown;
  const pending = callFull(page, "Held").then((r) => (got = r));
  await held.arrived();
  await settle(page);
  expect([script.count(""), got]).toEqual([0, undefined]); // parked: the script has not been asked, nothing answered
  held.release();
  await pending;
  expect(got).toEqual([200, JSON.stringify({ from: "script" })]); // released: the script's own reply, not the stub's
  expect(script.count("")).toBe(1);
  await call(page, "Held"); // after release: goes straight to the script, the hold counts nothing more
  expect([script.count(""), held.count()]).toEqual([2, 1]);
});

test("scriptByKey + holdRequests: registered the other way round the script answers first and the hold parks nothing", async ({ page }) => {
  const held = await holdRequests(page, { method: "Thing" });
  const script = await scriptByKey(page, RPC("Thing"), { "": { ok: { from: "script" } } }); // registered later: sees the call first
  expect(await callFull(page, "Thing")).toEqual([200, JSON.stringify({ from: "script" })]);
  expect([script.count(""), held.count()]).toEqual([1, 0]);
});

// ---- watchRequests -------------------------------------------------------------------------------------

test("watchRequests: counts what the browser sent, including a call a hold parks, and a parked call is counted once", async ({ page }) => {
  const seen = watchRequests(page, RPC("Sent"));
  const held = await holdRequests(page, { method: "Sent" });
  const pending = call(page, "Sent", OPS_KEY);
  await held.arrived();
  await expect.poll(() => seen.sent(OPS_KEY)).toBe(1); // sent, parked, not answered
  expect(held.count()).toBe(1);
  held.release();
  await pending;
  expect(seen.sent(OPS_KEY)).toBe(1); // answering it does not add a second one
  await call(page, "Sent", OPS_KEY);
  expect(seen.sent(OPS_KEY)).toBe(2); // a later call does, parked or not
});

test("watchRequests: a call failRequests refuses, or drops, was still sent", async ({ page }) => {
  const seen = watchRequests(page, RPC("Faulty"));
  await failRequests(page, { method: "Faulty", times: 2, mode: "unavailable" });
  expect(await call(page, "Faulty")).toBe(503);
  await failRequests(page, { method: "Faulty", mode: "reset" });
  expect(await call(page, "Faulty")).toBe("failed");
  expect(seen.sent("")).toBe(2);
});

test("watchRequests: the key separates callers, and \"\" is the call with no X-API-Key", async ({ page }) => {
  const seen = watchRequests(page, RPC("Who"));
  await call(page, "Who");
  await call(page, "Who", OPS_KEY);
  await call(page, "Who", OPS_KEY);
  await call(page, "Who", OTHER_KEY);
  expect([seen.sent(""), seen.sent(OPS_KEY), seen.sent(OTHER_KEY), seen.sent(ADMIN_KEY)]).toEqual([1, 2, 1, 0]);
});

test("watchRequests: text narrows to bodies containing it, case-sensitively; no text counts every body", async ({ page }) => {
  const seen = watchRequests(page, RPC("Body"));
  await call(page, "Body", OPS_KEY, '{"skus":["SW-SKU"]}');
  await call(page, "Body", OPS_KEY, '{"skus":["sw-sku"]}');
  await call(page, "Body", OPS_KEY, '{"skus":["OTHER"]}');
  expect([seen.sent(OPS_KEY, "SW-SKU"), seen.sent(OPS_KEY, "sw-sku"), seen.sent(OPS_KEY, "NOPE"), seen.sent(OPS_KEY)]).toEqual([1, 1, 0, 3]);
  expect(seen.sent(OTHER_KEY, "SW-SKU")).toBe(0); // the text must be in a call of THAT key
});

test("watchRequests: only the exact rpc; another method, a same-prefix name and a same-suffix name are not counted", async ({ page }) => {
  const seen = watchRequests(page, RPC("Mine"));
  await call(page, "MineExtra");
  await call(page, "XMine");
  await call(page, "Other");
  expect(seen.sent("")).toBe(0);
  await call(page, "Mine");
  expect(seen.sent("")).toBe(1);
});

// ---- extensions for the transient-recovery spec (#483): bodyIncludes and mode "permission_denied" ----
const callBody = (page: Page, method: string, body: string, key?: string) =>
  page.evaluate(
    ([m, b, k]) =>
      fetch(`/api/test.v1.TestService/${m}`, { method: "POST", headers: { "Content-Type": "application/json", ...(k ? { "X-API-Key": k } : {}) }, body: b })
        .then((r) => r.status)
        .catch(() => "failed"),
    [method, body, key] as const,
  );

test("failRequests: bodyIncludes spends the fault only on a call whose body contains the text (case sensitive)", async ({ page }) => {
  const f = await failRequests(page, { method: "Quote", bodyIncludes: "SKU-9" });
  expect(await callBody(page, "Quote", '{"skus":["SKU-1"]}')).toBe(200); // the default SKU the form quotes first: not matched, not counted
  expect(await callBody(page, "Quote", '{"skus":["sku-9"]}')).toBe(200); // case sensitive
  expect(await callBody(page, "Quote", "{}")).toBe(200); // a body with nothing in it
  expect(f.matched()).toBe(0);
  expect(await callBody(page, "Quote", '{"skus":["SKU-9"]}')).toBe(503); // the first match takes the fault ...
  expect(await callBody(page, "Quote", '{"skus":["SKU-9"]}')).toBe(200); // ... times defaults to 1
  expect([f.matched(), f.failed()]).toEqual([2, 1]);
});

test("failRequests: bodyIncludes combines with key and with method (all three must match)", async ({ page }) => {
  const f = await failRequests(page, { method: "Both", key: OPS_KEY, bodyIncludes: "needle", times: 5 });
  expect(await callBody(page, "Both", "needle")).toBe(200); // right body, anonymous
  expect(await callBody(page, "Both", "hay", OPS_KEY)).toBe(200); // right key, wrong body
  expect(await callBody(page, "Other", "needle", OPS_KEY)).toBe(200); // right key and body, another method
  expect(f.matched()).toBe(0);
  expect(await callBody(page, "Both", "a needle in it", OPS_KEY)).toBe(503);
  expect([f.matched(), f.failed()]).toEqual([1, 1]);
});

test("failRequests: a call without a body never matches bodyIncludes", async ({ page }) => {
  const f = await failRequests(page, { method: "NoBody", bodyIncludes: "x" });
  expect(await page.evaluate(() => fetch("/api/test.v1.TestService/NoBody", { method: "POST" }).then((r) => r.status))).toBe(200);
  expect(f.matched()).toBe(0);
});

test("holdRequests: bodyIncludes parks only the matching body", async ({ page }) => {
  const held = await holdRequests(page, { method: "HeldQuote", bodyIncludes: "SKU-9" });
  expect(await callBody(page, "HeldQuote", '{"skus":["SKU-1"]}')).toBe(200); // not parked: answered at once
  await settle(page);
  expect(held.count()).toBe(0);
  let status: unknown;
  const pending = callBody(page, "HeldQuote", '{"skus":["SKU-9"]}').then((s) => (status = s));
  await held.arrived();
  expect([held.count(), status]).toEqual([1, undefined]);
  held.release();
  await pending;
  expect(status).toBe(200);
});

test("failRequests: mode permission_denied answers 403 with a Connect permission_denied error; times Infinity never stops", async ({ page }) => {
  const f = await failRequests(page, { method: "Denied", mode: "permission_denied", times: Infinity });
  const answers = await page.evaluate(async () => {
    const out: unknown[] = [];
    for (let i = 0; i < 4; i++) {
      const r = await fetch("/api/test.v1.TestService/Denied", { method: "POST", body: "{}" });
      out.push([r.status, await r.json()]);
    }
    return out;
  });
  expect(answers).toEqual(Array(4).fill([403, { code: "permission_denied", message: "injected by failRequests" }]));
  expect([f.matched(), f.failed()]).toEqual([4, 4]);
});

test("failRequests: times 4 fails exactly four calls, then passes", async ({ page }) => {
  const f = await failRequests(page, { method: "Four", times: 4 });
  const statuses: unknown[] = [];
  for (let i = 0; i < 6; i++) statuses.push(await call(page, "Four"));
  expect(statuses).toEqual([503, 503, 503, 503, 200, 200]);
  expect([f.matched(), f.failed()]).toEqual([6, 4]);
});

test("failRequests: mode invalid_argument answers 400 with a Connect invalid_argument error, not 403 or 503", async ({ page }) => {
  const f = await failRequests(page, { method: "Invalid", mode: "invalid_argument", times: 2 });
  const answers = await page.evaluate(async () => {
    const out: unknown[] = [];
    for (let i = 0; i < 3; i++) {
      const r = await fetch("/api/test.v1.TestService/Invalid", { method: "POST", body: "{}" });
      out.push(r.status);
    }
    const r = await fetch("/api/test.v1.TestService/Invalid", { method: "POST", body: "{}" });
    return [out, r.status];
  });
  expect(answers).toEqual([[400, 400, 200], 200]);
  expect([f.matched(), f.failed()]).toEqual([4, 2]);
});

test("failRequests: mode invalid_argument's body carries the Connect code invalid_argument", async ({ page }) => {
  await failRequests(page, { method: "InvalidBody", mode: "invalid_argument" });
  expect(await callFull(page, "InvalidBody")).toEqual([400, JSON.stringify({ code: "invalid_argument", message: "injected by failRequests" })]);
});
