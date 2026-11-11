import { expect, test, type Page } from "@playwright/test";
import { failRequests } from "./fail-requests";
import { withDB } from "./mode";
import { LIST_POISON, PAGE_BODY, PROBE_BODY } from "./probe";

// A read that fails ONCE for a reason that passes (the server restarting, a connection reset) must not
// leave the console showing an error forever; a read that fails because the answer is "no" must keep
// showing it, and must not be asked again. The rpc is the real bin/rpc: failRequests (e2e/fail-requests.ts)
// only breaks the calls a test names, everything else goes on to it. Real time, no fake clock: the backoff
// is the thing under test. Deadlines are generous against the 3.5s retry budget (src/transient.ts).
const RECOVERY_MS = 15_000;
// The Poison list is polled every 5 s (src/queries.ts poisonQuery), so "the list recovered by itself" only
// means the retry did it, not the next poll, if it is on screen sooner than that.
const BEFORE_NEXT_POLL_MS = 4_000;

const PAGE_LIST = { method: "ListPoison", bodyIncludes: PAGE_BODY } as const;

// What the Poison page shows once its list was answered: with a database a table (rows from other runs
// may exist), without one the server's own answer, admin.no_database. Never the injected failure.
async function expectListAnswered(page: Page, timeout: number) {
  if (withDB) {
    await expect(page.getByRole("row").filter({ hasText: /Nothing parked\.|^\d+/ }).first()).toBeVisible({ timeout });
    await expect(page.getByRole("alert")).toHaveCount(0);
    return;
  }
  await expect(page.getByRole("alert")).toContainText("admin.no_database", { timeout });
  await expect(page.getByRole("alert")).not.toContainText(/unavailable/i);
}

test("(a) the first Poison list fails once (503): the page recovers by itself", async ({ page }) => {
  const fault = await failRequests(page, { ...PAGE_LIST, times: 1 }); // the first call: 503

  await page.goto("/poison");
  await expectListAnswered(page, BEFORE_NEXT_POLL_MS);
  expect(fault.failed(), "the refusal was really served").toBe(1);
  expect(fault.matched(), "and the list was asked again").toBeGreaterThanOrEqual(2);
});

test("(a2) the first Poison list resets the connection: the page recovers by itself", async ({ page }) => {
  const fault = await failRequests(page, { ...PAGE_LIST, mode: "reset" });

  await page.goto("/poison");
  await expectListAnswered(page, BEFORE_NEXT_POLL_MS);
  expect(fault.failed()).toBe(1);
  expect(fault.matched()).toBeGreaterThanOrEqual(2);
});

// (c) and (d) are guards: the console never retried an answer or a mutation, and the shared retry policy
// must not start to. (c) is a read with no polling of its own, the detail of one parked message: the list
// is a stub (this server has nothing parked), so that there is a row to open.
test("(c) PermissionDenied is an answer: the detail read is not retried, the alert stays, it is asked once", async ({ page }) => {
  test.setTimeout(60_000);
  await page.route(`**/${LIST_POISON}`, (route) =>
    (route.request().postData() ?? "").includes(PROBE_BODY)
      ? route.fallback()
      : route.fulfill({
          contentType: "application/json",
          json: { messages: [{ boundedContext: "bc-x", offset: "1", uuid: "u-1", topic: "t.v1", handler: "h", reason: "r-1" }] },
        }),
  );
  const detail = await failRequests(page, { method: "GetPoison", mode: "permission_denied", times: Infinity });

  await page.goto("/poison");
  await page.getByRole("button", { name: "Show" }).click();
  await expect(page.getByRole("alert")).toContainText("PermissionDenied", { timeout: RECOVERY_MS });
  // Past the whole retry budget (3.5s): nothing asks again.
  await page.waitForTimeout(8_000);
  await expect(page.getByRole("alert")).toContainText("PermissionDenied");
  expect(detail.matched(), "GetPoison requests").toBe(1);
});

test("(d) a Prune that fails is not sent again by the console", async ({ page }) => {
  const prune = await failRequests(page, { method: "Prune", times: Infinity });

  await page.goto("/prune");
  await page.getByRole("button", { name: "Dry run", exact: true }).click();

  await expect(page.getByRole("alert")).toContainText(/unavailable/i, { timeout: RECOVERY_MS });
  await page.waitForTimeout(6_000); // past the retry budget: a retrying mutation would have sent again by now
  expect(prune.matched(), "Prune requests").toBe(1);
});

// ---- the admin probe (#483 review) -----------------------------------------------------------------------------------
// adminProbeQuery had `retry: 3` + its own delay (an answer such as InvalidArgument was asked four times). It now
// follows the one policy in src/transient.ts. The faults are aimed at the probe's body: the page's own list asks the same RPC.

test("(e) the admin probe answered InvalidArgument is an answer: asked once, never retried", async ({ page }) => {
  const probe = await failRequests(page, { method: "ListPoison", bodyIncludes: PROBE_BODY, mode: "invalid_argument", times: Infinity });
  await page.goto("/poison");
  await expect.poll(() => probe.matched(), { timeout: RECOVERY_MS }).toBe(1);
  await page.waitForTimeout(6_000); // past the retry budget (3.5s): a retrying probe would have asked again by now
  expect(probe.matched(), "ListPoison probe requests").toBe(1);
});

test("(f) the admin probe refused once with 503 is retried: the admin UI appears without a reload", async ({ page }) => {
  const probe = await failRequests(page, { method: "ListPoison", bodyIncludes: PROBE_BODY }); // the first probe: 503
  await page.goto("/poison");
  await expect(page.getByRole("link", { name: "Prune" })).toBeVisible({ timeout: RECOVERY_MS });
  expect([probe.failed(), probe.matched() >= 2], "refused once, then asked again").toEqual([1, true]);
});
