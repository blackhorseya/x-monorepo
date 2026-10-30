import { expect, test } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { seedProduct } from "../catalog";
import { createCard } from "../orders-ui";
import { uid } from "../uid";
import { useKey } from "./helpers";

// QuotePrices is open to every caller: the order form previews prices for a key with no role,
// the same numbers an admin sees, and previewing sends no CreateOrder.
test("a role-less caller previews catalog prices and totals", async ({ page, request }) => {
  const sku = `E2E-QR-${uid()}`;
  await seedProduct(request, sku, { headers: { "X-API-Key": ADMIN_KEY } }); // 1299 minor USD
  const created: string[] = [];
  const quoted: number[] = [];
  page.on("request", (r) => {
    if (r.url().includes("/ordering.v1.OrderService/CreateOrder")) created.push(r.url());
  });
  page.on("response", (r) => {
    if (r.url().includes("/catalog.v1.CatalogService/QuotePrices")) quoted.push(r.status());
  });

  await page.goto("/orders");
  await useKey(page, OPS_KEY);
  const card = createCard(page);
  await expect(card.getByLabel("Customer")).toBeDisabled(); // no role: this is not an admin session
  await card.getByLabel("SKU").fill(sku);
  await card.getByLabel("Qty").fill("3");

  await expect(card.getByTestId("line-price")).toHaveText("12.99 USD each");
  await expect(card.getByTestId("quote-total")).toHaveText("Estimated total 38.97 USD");
  expect(quoted.length).toBeGreaterThan(0);
  // (the first call, before the key was entered, is an anonymous 401; no call is refused for lack of a role)
  expect(quoted.at(-1)).toBe(200);
  expect(quoted).not.toContain(403);
  expect(created).toHaveLength(0);
});
