import { expect, test, type APIRequestContext } from "@playwright/test";
import { seedProduct } from "./catalog";
import { scriptByKey } from "./inject";
import { createCard } from "./orders-ui";
import { uid } from "./uid";

// A read that fails ONCE for a reason that passes (the server restarting, a connection reset) must not
// leave the console showing an error forever; a read that fails because the answer is "no" must keep
// showing it, and must not be asked again. The rpc is the real bin/rpc: scriptByKey only breaks the
// calls a test names (everything else "pass"es). Real time, no fake clock: the backoff and the 2s poll
// are the thing under test. Deadlines are generous against the 3.5s retry budget (src/transient.ts).
const RECOVERY_MS = 15_000;
const LIST = "ordering.v1.OrderService/ListOrders";

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
  const list = await scriptByKey(page, LIST, { "*": [{ connect: "unavailable" }, "pass"] });

  await page.goto(`/orders?customer=${o.customerId}`);
  await expect(page.getByRole("row").filter({ hasText: o.short })).toBeVisible({ timeout: RECOVERY_MS });
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(list.total(), "the refusal was really served").toBeGreaterThanOrEqual(2);
});

test("(a2) the first ListOrders resets the connection: the Orders card recovers by itself", async ({ page, request }) => {
  const o = await placeOrder(request);
  const list = await scriptByKey(page, LIST, { "*": ["reset", "pass"] });

  await page.goto(`/orders?customer=${o.customerId}`);
  await expect(page.getByRole("row").filter({ hasText: o.short })).toBeVisible({ timeout: RECOVERY_MS });
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(list.total()).toBeGreaterThanOrEqual(2);
});

test("(a3) ListOrders keeps failing past the retry budget: the alert shows, then a poll tick brings the list back", async ({ page, request }) => {
  const o = await placeOrder(request);
  // 4 refusals = the first request and its 3 retries (src/transient.ts): react-query gives up and shows the error.
  // Only the page's own poll can get the rows back after that (refreshFirstPage with nothing cached).
  const refusal = { connect: "unavailable" } as const;
  const list = await scriptByKey(page, LIST, { "*": [refusal, refusal, refusal, refusal, "pass"] });

  await page.goto(`/orders?customer=${o.customerId}`);
  await expect(page.getByRole("alert")).toContainText(/unavailable/i, { timeout: RECOVERY_MS });
  await expect(page.getByRole("row").filter({ hasText: o.short })).toBeVisible({ timeout: RECOVERY_MS });
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(list.total()).toBeGreaterThanOrEqual(5);
});

test("(b) the first QuotePrices fails once: the Create order line shows its price", async ({ page, request }) => {
  const sku = `E2E-TR-${uid()}`;
  await seedProduct(request, sku); // 1299 minor USD
  // Only the call that names this SKU: the form quotes its default SKU first.
  const quote = await scriptByKey(page, "catalog.v1.CatalogService/QuotePrices", { "*": [{ connect: "unavailable" }, "pass"] }, { only: sku });

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").first().fill(sku);
  await card.getByLabel("Qty").first().fill("1");

  await expect(card.getByTestId("line-price").first()).toContainText("12.99 USD", { timeout: RECOVERY_MS });
  expect(quote.total(), "the refusal was really served").toBeGreaterThanOrEqual(2);
});

// (c) and (d) are guards, green before the fix too: the console never retried an answer or a mutation, and the
// fix must not start to. They are proven by mutants (retry everything / retry mutations), not by a red run.
test("(c) PermissionDenied is an answer: not retried, the alert stays, ListOrders is asked once", async ({ page, request }) => {
  const o = await placeOrder(request);
  const list = await scriptByKey(page, LIST, { "*": { connect: "permission_denied" } });

  await page.goto(`/orders?customer=${o.customerId}`);
  await expect(page.getByRole("alert")).toContainText("PermissionDenied", { timeout: RECOVERY_MS });
  // Past the whole retry budget (3.5s) and at least two 2s poll ticks: nothing asks again.
  await page.waitForTimeout(8_000);
  await expect(page.getByRole("alert")).toContainText("PermissionDenied");
  expect(list.total(), "ListOrders requests").toBe(1);
});

test("(d) a CreateOrder that fails is not sent again by the console", async ({ page, request }) => {
  const sku = `E2E-TR-${uid()}`;
  await seedProduct(request, sku);
  const create = await scriptByKey(page, "ordering.v1.OrderService/CreateOrder", { "*": { connect: "unavailable" } });

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").first().fill(sku);
  await expect(card.getByTestId("line-price").first()).toContainText("12.99 USD");
  await page.getByRole("button", { name: "Create order" }).click();

  await expect(page.getByRole("alert")).toContainText(/unavailable/i, { timeout: RECOVERY_MS });
  await page.waitForTimeout(6_000); // past the retry budget: a retrying mutation would have sent again by now
  expect(create.total(), "CreateOrder requests").toBe(1);
});
