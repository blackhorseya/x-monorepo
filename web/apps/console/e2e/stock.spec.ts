import { expect, test } from "@playwright/test";

const sku = `E2E-${Date.now()}`;

test("set stock, filter by prefix and by reserved", async ({ page }) => {
  await page.goto("/stock");
  await page.getByLabel("SKU", { exact: true }).fill(sku);
  await page.getByLabel("On hand").fill("7");
  await page.getByRole("button", { name: "Set", exact: true }).click();

  const row = page.getByRole("row").filter({ hasText: sku });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText("7");

  await page.getByLabel("SKU prefix").fill(sku);
  await page.getByRole("button", { name: "Filter" }).click();
  await expect(page).toHaveURL(new RegExp(`prefix=${sku}`));
  await expect(page.getByRole("row").filter({ hasText: /^E2E-|^SKU-/ })).toHaveCount(1);

  // Nothing holds this SKU (no worker reserves), so reserved-only hides it.
  await page.getByRole("button", { name: "Reserved only" }).click();
  await expect(page).toHaveURL(/reserved=true/);
  await expect(row).toHaveCount(0);
  await page.getByRole("button", { name: "Reserved only" }).click();
  await expect(page).not.toHaveURL(/reserved=/);
  await expect(row).toHaveCount(1);
});

test("a filtered link opens prefilled", async ({ page }) => {
  await page.goto(`/stock?prefix=${sku}&reserved=true`);
  await expect(page.getByLabel("SKU prefix")).toHaveValue(sku);
  await expect(page.getByRole("button", { name: "Clear" })).toBeVisible();
});
