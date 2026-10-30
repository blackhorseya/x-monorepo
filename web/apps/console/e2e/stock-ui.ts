import { expect, type Page } from "@playwright/test";

// The Set stock form picks its SKU from the catalog's products: narrow the list by prefix (the
// products are paged, and a database reused across runs holds more than one page), then choose.
export async function pickProduct(page: Page, sku: string) {
  await page.getByLabel("Find product").fill(sku);
  await page.getByRole("button", { name: "Find", exact: true }).click();
  await page.getByLabel("SKU", { exact: true }).selectOption({ value: sku });
}

// Set one SKU's on-hand count through the form. The SKU has to be a product already.
export async function setStock(page: Page, sku: string, onHand: number) {
  await pickProduct(page, sku);
  await page.getByLabel("On hand").fill(String(onHand));
  await page.getByRole("button", { name: "Set", exact: true }).click();
  await expect(page.getByRole("row").filter({ hasText: sku })).toHaveCount(1);
}
