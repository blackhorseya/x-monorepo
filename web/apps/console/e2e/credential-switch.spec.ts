import { expect, test, type Page } from "@playwright/test";
import { settle, useKey } from "./auth/helpers";
import { scriptByKey, type Reply } from "./inject";
import { createCard } from "./orders-ui";

// #483. Switching credential must re-ask every query the page is showing, with the NEW credential, and
// must not show what was read for the old one. Scripted rpc (e2e/inject.ts): each credential gets its own
// answer and the answer for the old one can be held back until after the switch, so the interleavings
// are made by the test and not by timing.

const orderOf = (id: string, customerId: string) => ({
  id,
  customerId,
  items: [{ sku: "SKU-1", quantity: 1 }],
  total: { amountMinor: "1299", currency: "USD" },
  status: "ORDER_STATUS_PENDING",
});
const listOf = (...ids: string[]): Reply => ({ ok: { orders: ids.map((id) => orderOf(id, "cust-sw")), nextPageToken: "" } });
// The list shows the LAST 8 characters of an id (the first ones are the v7 timestamp).
const rowOf = (page: Page, id: string) => page.getByRole("row").filter({ hasText: id.slice(-8) });
const ID_A = "aaaaaaaa-0000-7000-8000-000000000001";
const ID_OPS = "bbbbbbbb-0000-7000-8000-000000000002";
const PRICES_OPS: Reply = { ok: { prices: { "SW-SKU": { amountMinor: "1299", currency: "USD" } }, missing: [], inactive: [] } };

test("ListOrders: the anonymous call is still in flight when the key is set; the page shows the key's rows, not the 401", async ({ page }) => {
  const list = await scriptByKey(page, "ordering.v1.OrderService/ListOrders", {
    "": "hold",
    OPS: listOf(ID_OPS),
  });
  await page.goto("/orders?customer=cust-sw");
  await expect.poll(() => list.isHeld(""), { message: "the anonymous ListOrders is in flight" }).toBe(true);

  await useKey(page, "OPS");
  await list.release("", { connect: "unauthenticated" }); // the old answer arrives after the switch

  await expect(rowOf(page, ID_OPS)).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(list.count("OPS"), "ListOrders sent with the new key").toBeGreaterThanOrEqual(1);
});

test("QuotePrices: the anonymous call is still in flight when the key is set; the line shows the key's price, not 'price unavailable'", async ({ page }) => {
  const quote = await scriptByKey(page, "catalog.v1.CatalogService/QuotePrices", {
    "": "hold", // the form's default SKU is quoted first and held too; the one this test is about names SW-SKU
    OPS: PRICES_OPS,
  });
  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").first().fill("SW-SKU");
  await card.getByLabel("Qty").first().fill("1");
  await expect.poll(() => quote.isHeld("", "SW-SKU"), { message: "the anonymous QuotePrices for SW-SKU is in flight" }).toBe(true);

  await useKey(page, "OPS");
  await quote.release("", { connect: "unauthenticated" });

  await expect(card.getByTestId("line-price").first()).toContainText("12.99 USD");
  expect(quote.count("OPS"), "QuotePrices sent with the new key").toBeGreaterThanOrEqual(1);
});

test("a reply for the old credential that arrives after the switch is not shown, and the new credential is asked", async ({ page }) => {
  // The anonymous call fails (the list has no data), A's call is held, and the page switches again to B:
  // a query with no data and a call in flight is the case where a second request used to be folded into
  // the first. B's rows must come from a call B made, and A's late rows must never be shown.
  const list = await scriptByKey(page, "ordering.v1.OrderService/ListOrders", {
    "": { connect: "unauthenticated" },
    A: "hold",
    B: listOf(ID_OPS),
  });
  await page.goto("/orders?customer=cust-sw");
  await expect(page.getByRole("alert")).toContainText("Not authenticated");
  await useKey(page, "A");
  await expect.poll(() => list.isHeld("A"), { message: "A's ListOrders is in flight" }).toBe(true);

  await useKey(page, "B");
  await expect(rowOf(page, ID_OPS)).toBeVisible();
  await list.release("A", listOf(ID_A)); // A's rows arrive late

  await page.waitForTimeout(500); // a late write would be on screen long before this
  await expect(rowOf(page, ID_A)).toHaveCount(0);
  await expect(rowOf(page, ID_OPS)).toBeVisible();
  expect(list.count("B"), "ListOrders sent with B").toBeGreaterThanOrEqual(1);
});

test("rows read for the old credential do not stay on screen while the new credential's answer is awaited", async ({ page }) => {
  const list = await scriptByKey(page, "ordering.v1.OrderService/ListOrders", {
    "": listOf(),
    A: listOf(ID_A),
    B: "hold",
  });
  await page.goto("/orders?customer=cust-sw");
  await expect.poll(() => list.count(""), { message: "the anonymous ListOrders was answered" }).toBe(1);
  await settle(page);
  await useKey(page, "A");
  await expect(rowOf(page, ID_A)).toBeVisible();

  await useKey(page, "B");
  await expect.poll(() => list.isHeld("B"), { message: "B's ListOrders is in flight" }).toBe(true);
  await expect(rowOf(page, ID_A), "A's rows while B's answer is pending").toHaveCount(0);

  await list.release("B", listOf(ID_OPS));
  await expect(rowOf(page, ID_OPS)).toBeVisible();
});

// A reply is "late" when the credential it was asked with is no longer the one in use. The response is awaited
// in the browser (not slept for) and the page is given its frames to react before the caller asserts that
// nothing of it is shown: a negative assertion made after a provable delivery, not after a guessed delay.
async function deliverLate(page: Page, rpc: string, key: string, release: () => Promise<void>) {
  const arrived = page.waitForResponse((r) => r.url().endsWith(rpc) && (r.request().headers()["x-api-key"] ?? "") === key);
  await release();
  await arrived;
  await settle(page);
}

test("ListOrders: A's rows arrive late while B's call is still pending; they are not shown, B's rows are", async ({ page }) => {
  const list = await scriptByKey(page, "ordering.v1.OrderService/ListOrders", {
    "": { connect: "unauthenticated" },
    A: "hold",
    B: "hold",
  });
  await page.goto("/orders?customer=cust-sw");
  await expect(page.getByRole("alert")).toContainText("Not authenticated");
  await useKey(page, "A");
  await expect.poll(() => list.isHeld("A"), { message: "A's ListOrders is in flight" }).toBe(true);
  await useKey(page, "B");
  await settle(page);

  await deliverLate(page, "ordering.v1.OrderService/ListOrders", "A", () => list.release("A", listOf(ID_A)));
  await expect(rowOf(page, ID_A), "A's rows after A's late answer, B still pending").toHaveCount(0);
  // positive control: B's call was parked all along, so the empty page above is "B pending", not "nothing asked"
  await expect.poll(() => list.isHeld("B"), { message: "B's ListOrders is in flight" }).toBe(true);

  await list.release("B", listOf(ID_OPS));
  await expect(rowOf(page, ID_OPS)).toBeVisible();
  await expect(rowOf(page, ID_A)).toHaveCount(0);
});

test("QuotePrices: A's price arrives late while B's call is still pending; it is not shown, B's is", async ({ page }) => {
  const RPC = "catalog.v1.CatalogService/QuotePrices";
  // The anonymous call is refused (not held), so the query is idle with an error when A is chosen: A's call is sent.
  const quote = await scriptByKey(page, RPC, { "": { connect: "unauthenticated" }, A: "hold", B: "hold" });
  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").first().fill("SW-SKU");
  await card.getByLabel("Qty").first().fill("1");
  await expect(card.getByTestId("line-price").first()).toContainText("price unavailable");
  await useKey(page, "A");
  await expect.poll(() => quote.isHeld("A", "SW-SKU"), { message: "A's QuotePrices is in flight" }).toBe(true);
  await useKey(page, "B");
  await settle(page);

  const priceA: Reply = { ok: { prices: { "SW-SKU": { amountMinor: "999", currency: "USD" } }, missing: [], inactive: [] } };
  await deliverLate(page, RPC, "A", () => quote.release("A", priceA));
  await expect(card.getByTestId("line-price").first(), "A's price after A's late answer, B still pending").not.toContainText("9.99");
  await expect.poll(() => quote.isHeld("B", "SW-SKU"), { message: "B's QuotePrices is in flight" }).toBe(true);

  await quote.release("B", PRICES_OPS);
  await expect(card.getByTestId("line-price").first()).toContainText("12.99 USD");
});

// Control for the hold itself: "held" means a request is parked right now. A release that finds nothing parked
// must not leave anything behind that a later request (or isHeld) could mistake for a caught one.
test("scriptByKey: a release before any request catches nothing; the request that comes later is parked, not answered", async ({ page }) => {
  const list = await scriptByKey(page, "ordering.v1.OrderService/ListOrders", { "": "hold" });
  await list.release("", listOf(ID_A)); // nothing parked yet
  expect(list.isHeld(""), "held after a release that had nothing to release").toBe(false);
  await page.goto("/orders?customer=cust-sw");
  await expect.poll(() => list.isHeld(""), { message: "the request made after the release is parked" }).toBe(true);
  await settle(page);
  await expect(rowOf(page, ID_A), "the early release must not answer the later request").toHaveCount(0);
});
