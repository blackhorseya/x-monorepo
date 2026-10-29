import { expect, test } from "@playwright/test";
import { uid } from "./uid";

// /products against the real server: with auth off the caller passes the admin gate, so this
// is the catalog's whole admin loop. The auth project (e2e/auth/) covers who may see the page.

const sku = `E2E-${uid()}`;
const other = `ZZ-${uid()}`;

test("save a product, reprice it, deactivate it, bring it back", async ({ page }) => {
  await page.goto("/products");
  await page.getByLabel("SKU", { exact: true }).fill(sku);
  await page.getByLabel("Name", { exact: true }).fill("Apple");
  await page.getByLabel("Price (minor units)").fill("1999");
  await page.getByLabel("Currency").fill("usd");
  await page.getByRole("button", { name: "Save product" }).click();

  // Narrow the list to this run's SKU first: a database reused across runs holds more than one page.
  await page.getByLabel("SKU prefix").fill(sku);
  await page.getByRole("button", { name: "Filter" }).click();
  const row = page.getByRole("row").filter({ hasText: sku });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText("Apple");
  await expect(row).toContainText("19.99 USD");
  await expect(row).toContainText("active");

  // Edit loads the row into the form; saving it again is a reprice, in another currency.
  await row.getByRole("button", { name: "Edit" }).click();
  await expect(page.getByLabel("Price (minor units)")).toHaveValue("1999");
  await page.getByLabel("Price (minor units)").fill("2500");
  await page.getByLabel("Currency").fill("EUR");
  await page.getByRole("button", { name: "Save product" }).click();
  await expect(row).toContainText("25.00 EUR");
  await expect(row).toHaveCount(1);

  await row.getByRole("button", { name: "Deactivate" }).click();
  await expect(row).toContainText("inactive");
  await expect(row.getByRole("button", { name: "Deactivate" })).toHaveCount(0);

  // Saving a deactivated product puts it back on sale.
  await row.getByRole("button", { name: "Edit" }).click();
  await page.getByRole("button", { name: "Save product" }).click();
  await expect(row).not.toContainText("inactive");
  await expect(row.getByRole("button", { name: "Deactivate" })).toBeVisible();
});

test("a bad price is refused by the server and shown", async ({ page }) => {
  await page.goto("/products");
  await page.getByLabel("SKU", { exact: true }).fill(`BAD-${uid()}`);
  await page.getByLabel("Name", { exact: true }).fill("Broken");
  await page.getByLabel("Price (minor units)").fill("100");
  await page.getByLabel("Currency").fill("US"); // not ISO 4217
  await page.getByRole("button", { name: "Save product" }).click();
  await expect(page.getByRole("alert")).toContainText("request.validation_failed");
});

test("filter by prefix, and a filtered link opens prefilled", async ({ page }) => {
  await page.goto("/products");
  await page.getByLabel("SKU", { exact: true }).fill(other);
  await page.getByLabel("Name", { exact: true }).fill("Zed");
  await page.getByLabel("Price (minor units)").fill("5");
  await page.getByRole("button", { name: "Save product" }).click();

  await page.getByLabel("SKU prefix").fill(other);
  await page.getByRole("button", { name: "Filter" }).click();
  await expect(page).toHaveURL(new RegExp(`prefix=${other}`));
  await expect(page.getByRole("row").filter({ hasText: other })).toHaveCount(1);
  await expect(page.getByRole("row").filter({ hasText: sku })).toHaveCount(0);

  await page.goto(`/products?prefix=${other}`);
  await expect(page.getByLabel("SKU prefix")).toHaveValue(other);
  await expect(page.getByRole("button", { name: "Clear" })).toBeVisible();
});
