import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { failRequests, holdRequests } from "../fail-requests";
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
const call = (page: Page, method: string, key?: string) =>
  page.evaluate(
    ([m, k]) =>
      fetch(`/api/test.v1.TestService/${m}`, { method: "POST", headers: { "Content-Type": "application/json", ...(k ? { "X-API-Key": k } : {}) }, body: "{}" })
        .then((r) => r.status)
        .catch(() => "failed"),
    [method, key] as const,
  );

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
  const f = await failRequests(page, { method: "ListOrders" });
  expect(await call(page, "ListOrdersExtra")).toBe(200); // another method, even with the same prefix
  expect(await call(page, "XListOrders")).toBe(200); // ... or one that merely contains it
  expect(await call(page, "ListOrder")).toBe(200);
  expect(await call(page, "ListAccounts")).toBe(200); // another RPC altogether
  expect(f.matched()).toBe(0);
  expect(await call(page, "ListOrders")).toBe(503); // the first
  expect(await call(page, "ListOrders")).toBe(200); // times defaults to 1
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
  expect(await call(page, "ListOrders")).toBe(200);
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
