import { expect, test, type Page } from "@playwright/test";
import { seedProduct } from "./catalog";
import { uid } from "./uid";
import { createCard } from "./orders-ui";
import { settle } from "./auth/helpers";

// Every CreateOrder request the page sends, as the body it carried: a preview must leave this empty.
function createOrderRequests(page: Page): string[] {
  const sent: string[] = [];
  page.on("request", (r) => {
    if (r.url().includes("/ordering.v1.OrderService/CreateOrder")) sent.push(r.postData() ?? "");
  });
  return sent;
}

const quotedSkus = (route: { request(): { postDataJSON(): { skus: string[] } } }) => route.request().postDataJSON().skus;

// The order form does not take a price: it shows the one the catalog would charge right now
// (CatalogService.QuotePrices), read-only, before anything is submitted. What the order is
// finally charged is decided by CreateOrder; this is only the preview of it.

test("the form shows the catalog price of each line and the total before submitting", async ({ page, request }) => {
  const sku = `E2E-Q-${uid()}`;
  await seedProduct(request, sku); // 1299 minor USD

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").fill(sku);
  await card.getByLabel("Qty").fill("2");

  await expect(card.getByTestId("line-price")).toContainText("12.99 USD");
  await expect(card.getByTestId("quote-total")).toContainText("25.98 USD");

  // a quantity change re-prices the total, from the same quote
  await card.getByLabel("Qty").fill("3");
  await expect(card.getByTestId("quote-total")).toContainText("38.97 USD");
});

test("a SKU the catalog cannot quote says why, and the total waits", async ({ page, request }) => {
  const live = `E2E-Q-${uid()}`;
  const gone = `E2E-Q-${uid()}`;
  await seedProduct(request, live);
  await seedProduct(request, gone);
  const off = await request.post("/api/catalog.v1.CatalogService/DeactivateProduct", { data: { sku: gone } });
  expect(off.ok()).toBe(true);

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").first().fill(`E2E-Q-nobody-${uid()}`);
  await expect(card.getByTestId("line-price").first()).toContainText("not in the catalog");

  await card.getByLabel("SKU").first().fill(gone);
  await expect(card.getByTestId("line-price").first()).toContainText("not on sale");
  await expect(card.getByTestId("quote-total")).not.toContainText("USD");

  await card.getByLabel("SKU").first().fill(live);
  await expect(card.getByTestId("quote-total")).toContainText("25.98 USD"); // default quantity 2
});

test("lines priced in two currencies are flagged before the server refuses them", async ({ page, request }) => {
  const usd = `E2E-Q-${uid()}`;
  const eur = `E2E-Q-${uid()}`;
  await seedProduct(request, usd);
  await seedProduct(request, eur, { currency: "EUR" });

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").first().fill(usd);
  await page.getByRole("button", { name: "Line", exact: true }).click();
  await card.getByLabel("SKU").nth(1).fill(eur);

  await expect(card.getByTestId("quote-total")).toContainText("one currency");
});

// A catalog SKU is an exact key: "A" and " A " are two products. The preview prices the very
// string the order will carry, never a tidied-up one.
test("the preview prices the exact SKU that will be submitted", async ({ page, request }) => {
  const plain = `E2E-Q-${uid()}`;
  const spaced = ` ${plain} `;
  await seedProduct(request, plain); // 1299 minor USD
  await seedProduct(request, spaced, { amountMinor: 9999 });

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").fill(spaced);
  await card.getByLabel("Qty").fill("2");
  await expect(card.getByTestId("line-price")).toContainText("99.99 USD");
  await expect(card.getByTestId("quote-total")).toContainText("199.98 USD");

  await card.getByLabel("SKU").fill(plain);
  await expect(card.getByTestId("line-price")).toContainText("12.99 USD");
});

test("a SKU that only exists without the spaces is not quoted as if it did", async ({ page, request }) => {
  const plain = `E2E-Q-${uid()}`;
  await seedProduct(request, plain);

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").fill(`${plain} `);
  await expect(card.getByTestId("line-price")).toContainText("not in the catalog");
  await expect(card.getByTestId("quote-total")).not.toContainText("USD");
});

// A quote that cannot be refreshed is not shown as the current price, even though an older
// answer for the same SKU is still in the cache.
test("a failed refresh of a cached quote says the price is unavailable", async ({ page, request }) => {
  const a = `E2E-Q-${uid()}`;
  const b = `E2E-Q-${uid()}`;
  await seedProduct(request, a);
  await seedProduct(request, b, { amountMinor: 2599 });
  let failing = false;
  await page.route("**/catalog.v1.CatalogService/QuotePrices", (route) =>
    failing ? route.fulfill({ status: 503, contentType: "application/json", body: '{"code":"unavailable","message":"down"}' }) : route.continue(),
  );

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").fill(a);
  await card.getByLabel("Qty").fill("2");
  await expect(card.getByTestId("quote-total")).toContainText("25.98 USD");
  await card.getByLabel("SKU").fill(b);
  await expect(card.getByTestId("line-price")).toContainText("25.99 USD");

  await page.waitForTimeout(1500); // past the cache's freshness: asking about a again goes to the server
  failing = true;
  await card.getByLabel("SKU").fill(a);
  await expect(card.getByTestId("line-price")).toContainText("price unavailable");
  await expect(card.getByTestId("quote-total")).not.toContainText("USD");
});

// With nothing cached for the SKU, a quote that cannot be had is "unavailable", not a number.
test("a first quote that fails says the price is unavailable and shows no total", async ({ page, request }) => {
  const sku = `E2E-Q-${uid()}`;
  await seedProduct(request, sku);
  await page.route("**/catalog.v1.CatalogService/QuotePrices", (route) =>
    route.fulfill({ status: 503, contentType: "application/json", body: '{"code":"unavailable","message":"down"}' }),
  );
  const orders = createOrderRequests(page);

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").fill(sku);
  await card.getByLabel("Qty").fill("2");
  await expect(card.getByTestId("line-price")).toHaveText("price unavailable");
  await expect(card.getByTestId("quote-total")).toHaveText("Total once every line has a price");
  await expect(card.getByTestId("line-price")).not.toContainText("12.99");
  await expect(card.getByTestId("quote-total")).not.toContainText("25.98");
  expect(orders).toHaveLength(0);
});

// An answer that arrives after the user moved on to another SKU is not the current quote.
test("a late answer for the previous SKU does not overwrite the preview of the current one", async ({ page, request }) => {
  const a = `E2E-Q-${uid()}`;
  const b = `E2E-Q-${uid()}`;
  await seedProduct(request, a); // 1299 minor USD
  await seedProduct(request, b, { amountMinor: 2599 });
  let aAsked!: () => void;
  const aWasAsked = new Promise<void>((resolve) => (aAsked = resolve));
  let releaseA!: () => void;
  const aMayAnswer = new Promise<void>((resolve) => (releaseA = resolve));
  await page.route("**/catalog.v1.CatalogService/QuotePrices", async (route) => {
    if (!quotedSkus(route).includes(a)) return route.continue();
    const real = await route.fetch(); // the true answer for a, held back until b has been shown
    aAsked();
    await aMayAnswer;
    await route.fulfill({ response: real });
  });
  const orders = createOrderRequests(page);

  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("Qty").fill("2");
  await card.getByLabel("SKU").fill(a);
  await aWasAsked;
  await card.getByLabel("SKU").fill(b);
  await expect(card.getByTestId("line-price")).toHaveText("25.99 USD each");
  await expect(card.getByTestId("quote-total")).toHaveText("Estimated total 51.98 USD");

  const aAnswered = page.waitForResponse((r) => r.url().includes("QuotePrices") && (r.request().postDataJSON().skus as string[]).includes(a));
  releaseA();
  await aAnswered;
  await settle(page);
  await expect(card.getByTestId("line-price")).toHaveText("25.99 USD each");
  await expect(card.getByTestId("quote-total")).toHaveText("Estimated total 51.98 USD");

  // and going back to a prices it from its own answer
  await card.getByLabel("SKU").fill(a);
  await expect(card.getByTestId("line-price")).toHaveText("12.99 USD each");
  await expect(card.getByTestId("quote-total")).toHaveText("Estimated total 25.98 USD");
  expect(orders).toHaveLength(0);
});

// A SKU made only of a space is a legal catalog key (nothing trims it); created through the
// Products page, the order form quotes it and submits that same string.
test("a product whose SKU is a single space, saved on Products, is quoted and ordered as that SKU", async ({ page }) => {
  await page.goto("/products");
  await page.getByLabel("SKU", { exact: true }).fill(" ");
  await page.getByLabel("Name", { exact: true }).fill(`Blank ${uid()}`);
  await page.getByLabel("Price (minor units)").fill("765");
  await page.getByLabel("Currency").fill("USD");
  await page.getByRole("button", { name: "Save product" }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(page.getByRole("row").filter({ hasText: "7.65 USD" }).first()).toBeVisible();

  const orders = createOrderRequests(page);
  await page.goto("/orders");
  const card = createCard(page);
  await card.getByLabel("SKU").fill(" ");
  await card.getByLabel("Qty").fill("2");
  await expect(card.getByTestId("line-price")).toHaveText("7.65 USD each");
  await expect(card.getByTestId("quote-total")).toHaveText("Estimated total 15.30 USD");
  expect(orders).toHaveLength(0); // previewing is not ordering

  await card.getByRole("button", { name: "Create order" }).click();
  await expect.poll(() => orders.length).toBe(1);
  expect(JSON.parse(orders[0]).items).toEqual([{ sku: " ", quantity: 2 }]);
});
