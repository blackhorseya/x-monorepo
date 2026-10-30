import { expect, test } from "@playwright/test";
import { seedProduct } from "./catalog";
import { uid } from "./uid";
import { createCard } from "./orders-ui";

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
