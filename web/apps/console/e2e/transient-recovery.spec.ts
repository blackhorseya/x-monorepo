import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { seedProduct } from "./catalog";
import { failRequests } from "./fail-requests";
import { createCard } from "./orders-ui";
import { uid } from "./uid";

// A read that fails ONCE for a reason that passes (the server restarting, a connection reset) must not
// leave the console showing an error forever; a read that fails because the answer is "no" must keep
// showing it, and must not be asked again. The rpc is the real bin/rpc: failRequests (e2e/fail-requests.ts)
// only breaks the calls a test names, everything else goes on to it. Real time, no fake clock: the backoff and the 2s poll
// are the thing under test. Deadlines are generous against the 3.5s retry budget (src/transient.ts).
const RECOVERY_MS = 15_000;

// One order of a customer of the test's own, so the list a test reads is its own rows. The list shows the
// LAST 8 characters of an id (the first ones are the v7 timestamp).
async function placeOrder(request: APIRequestContext) {
  const sku = `E2E-TR-${uid()}`;
  const customerId = `cust-tr-${uid()}`;
  await seedProduct(request, sku);
  const res = await request.post("/api/ordering.v1.OrderService/CreateOrder", {
    data: { customerId, items: [{ sku, quantity: 1 }], idempotencyKey: crypto.randomUUID() },
  });
  expect(res.ok(), `CreateOrder: ${res.status()} ${await res.text()}`).toBe(true);
  const id = ((await res.json()) as { order: { id: string } }).order.id;
  return { sku, customerId, short: id.slice(-8) };
}

test("(a) the first ListOrders fails once (503): the Orders card recovers by itself", async ({ page, request }) => {
  const o = await placeOrder(request);
  const fault = await failRequests(page, { method: "ListOrders" }); // the first call: 503

  await page.goto(`/orders?customer=${o.customerId}`);
  await expect(page.getByRole("row").filter({ hasText: o.short })).toBeVisible({ timeout: RECOVERY_MS });
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(fault.failed(), "the refusal was really served").toBe(1);
  expect(fault.matched(), "and the list was asked again").toBeGreaterThanOrEqual(2);
});

test("(a2) the first ListOrders resets the connection: the Orders card recovers by itself", async ({ page, request }) => {
  const o = await placeOrder(request);
  const fault = await failRequests(page, { method: "ListOrders", mode: "reset" });

  await page.goto(`/orders?customer=${o.customerId}`);
  await expect(page.getByRole("row").filter({ hasText: o.short })).toBeVisible({ timeout: RECOVERY_MS });
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(fault.failed()).toBe(1);
  expect(fault.matched()).toBeGreaterThanOrEqual(2);
});

test("(a3) ListOrders keeps failing past the retry budget: the alert shows, then a poll tick brings the list back", async ({ page, request }) => {
  const o = await placeOrder(request);
  // 4 refusals = the first request and its 3 retries (src/transient.ts): react-query gives up and shows the error.
  // Only the page's own poll can get the rows back after that (refreshFirstPage with nothing cached).
  const fault = await failRequests(page, { method: "ListOrders", times: 4 });
  // The alert is on screen only from the last refusal to the poll's next refetch (a query with no data goes back to
  // "pending" when it refetches, which clears the error): at most one 2s tick, shorter than a polling assertion can
  // be trusted to sample. So the page records every alert text it ever showed, and the test reads that afterwards.
  await page.addInitScript(() => {
    const seen: string[] = [];
    (window as unknown as { __alerts: string[] }).__alerts = seen;
    new MutationObserver(() => {
      for (const el of document.querySelectorAll('[role="alert"]')) if (el.textContent) seen.push(el.textContent);
    }).observe(document, { subtree: true, childList: true, characterData: true });
  });

  await page.goto(`/orders?customer=${o.customerId}`);
  await expect(page.getByRole("row").filter({ hasText: o.short })).toBeVisible({ timeout: RECOVERY_MS });
  await expect(page.getByRole("alert")).toHaveCount(0);
  const alerts = await page.evaluate(() => (window as unknown as { __alerts: string[] }).__alerts);
  expect(alerts.join("\n"), "the error was shown before the list came back").toMatch(/unavailable/i);
  expect(fault.failed()).toBe(4);
  expect(fault.matched()).toBeGreaterThanOrEqual(5);
});

test("(b) the first QuotePrices fails once: the Create order line shows its price", async ({ page, request }) => {
  const sku = `E2E-TR-${uid()}`;
  await seedProduct(request, sku); // 1299 minor USD
  // Only the call that names this SKU: the form quotes its default SKU first.
  const quote = await failRequests(page, { method: "QuotePrices", bodyIncludes: sku });

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").first().fill(sku);
  await card.getByLabel("Qty").first().fill("1");

  await expect(card.getByTestId("line-price").first()).toContainText("12.99 USD", { timeout: RECOVERY_MS });
  expect(quote.failed(), "the refusal was really served").toBe(1);
  expect(quote.matched(), "and the SKU was quoted again").toBeGreaterThanOrEqual(2);
});

// (c) and (d) are guards, green before the fix too: the console never retried an answer or a mutation, and the
// fix must not start to. They are proven by mutants (retry everything / retry mutations), not by a red run.
test("(c) PermissionDenied is an answer: not retried, the alert stays, ListOrders is asked once", async ({ page, request }) => {
  const o = await placeOrder(request);
  const list = await failRequests(page, { method: "ListOrders", mode: "permission_denied", times: Infinity });

  await page.goto(`/orders?customer=${o.customerId}`);
  await expect(page.getByRole("alert")).toContainText("PermissionDenied", { timeout: RECOVERY_MS });
  // Past the whole retry budget (3.5s) and at least two 2s poll ticks: nothing asks again.
  await page.waitForTimeout(8_000);
  await expect(page.getByRole("alert")).toContainText("PermissionDenied");
  expect(list.matched(), "ListOrders requests").toBe(1);
});

test("(d) a CreateOrder that fails is not sent again by the console", async ({ page, request }) => {
  const sku = `E2E-TR-${uid()}`;
  await seedProduct(request, sku);
  const create = await failRequests(page, { method: "CreateOrder", times: Infinity });

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").first().fill(sku);
  await expect(card.getByTestId("line-price").first()).toContainText("12.99 USD");
  await page.getByRole("button", { name: "Create order" }).click();

  await expect(page.getByRole("alert")).toContainText(/unavailable/i, { timeout: RECOVERY_MS });
  await page.waitForTimeout(6_000); // past the retry budget: a retrying mutation would have sent again by now
  expect(create.matched(), "CreateOrder requests").toBe(1);
});

// ---- the queries that used to set their own retry (#483 review) ----------------------------------------------------
// adminProbeQuery had `retry: 3` + its own delay (an answer such as InvalidArgument was asked four times) and
// whoAmIQuery had `retry: false` (a 503 left the order form's Customer field locked for good). Both now follow
// the one policy in src/transient.ts; each cell has the mutant that puts the old line back.
const setButton = (page: Page) => page.getByRole("button", { name: "Set", exact: true });

test("(e) the admin probe answered InvalidArgument is an answer: asked once, never retried", async ({ page }) => {
  const probe = await failRequests(page, { method: "ListAccounts", mode: "invalid_argument", times: Infinity });
  await page.goto("/stock");
  await expect.poll(() => probe.matched(), { timeout: RECOVERY_MS }).toBe(1);
  await page.waitForTimeout(6_000); // past the retry budget (3.5s): a retrying probe would have asked again by now
  expect(probe.matched(), "ListAccounts requests").toBe(1);
});

test("(f) the admin probe refused once with 503 is retried: the admin UI appears without a reload", async ({ page }) => {
  const probe = await failRequests(page, { method: "ListAccounts" }); // the first call: 503
  await page.goto("/stock");
  await expect(setButton(page)).toBeVisible({ timeout: RECOVERY_MS });
  expect([probe.failed(), probe.matched() >= 2], "refused once, then asked again").toEqual([1, true]);
});

test("(g) WhoAmI refused once with 503 is retried: the order form's Customer field unlocks", async ({ page }) => {
  // The field is enabled for an admin or an anonymous caller. auth is off here, so the probe would make everyone an admin:
  // it is answered "not admin" (403), which leaves WhoAmI's `anonymous` as the only thing that can unlock the field.
  await failRequests(page, { method: "ListAccounts", mode: "permission_denied", times: Infinity });
  const who = await failRequests(page, { method: "WhoAmI" }); // the first call: 503
  await page.goto("/orders");
  await expect(createCard(page).getByLabel("Customer")).toBeEnabled({ timeout: RECOVERY_MS });
  expect([who.failed(), who.matched() >= 2], "refused once, then asked again").toEqual([1, true]);
});
