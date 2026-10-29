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
