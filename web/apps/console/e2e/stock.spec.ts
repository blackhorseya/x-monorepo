import { create, toBinary } from "@bufbuild/protobuf";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { ListStocksResponseSchema, type ListStocksResponse } from "@repo/proto/inventory/v1/service_pb";
import { StockSchema } from "@repo/proto/inventory/v1/entity_pb";
import { deactivateProduct, seedProduct } from "./catalog";
import { pickProduct, setStock } from "./stock-ui";
import { uid } from "./uid";

const setButton = (page: Page) => page.getByRole("button", { name: "Set", exact: true });
const skuField = (page: Page) => page.getByLabel("SKU", { exact: true });

// Stock rows the page is shown instead of the server's: for the states the server cannot be made
// to produce (inventory no longer creates a SKU the catalog does not know, so an orphan can only
// come from before that, or from a database edit). The catalog side stays real.
async function showStocks(page: Page, skus: string[]) {
  const body: ListStocksResponse = create(ListStocksResponseSchema, {
    stocks: skus.map((sku) => create(StockSchema, { sku, onHand: 4, reserved: 0, available: 4 })),
  });
  await page.route("**/inventory.v1.InventoryService/ListStocks", (route) =>
    route.fulfill({ status: 200, contentType: "application/proto", body: Buffer.from(toBinary(ListStocksResponseSchema, body)) }),
  );
}

async function setStockViaAPI(request: APIRequestContext, sku: string, onHand: number) {
  const res = await request.post("/api/inventory.v1.InventoryService/SetStock", { data: { sku, onHand } });
  expect(res.ok(), `SetStock ${sku}: ${res.status()} ${await res.text()}`).toBe(true);
}

const failing = (page: Page, rpc: string) =>
  page.route(`**/${rpc}`, (route) =>
    route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ code: "internal", message: "catalog is down" }) }),
  );

test("set stock for a catalog product, filter by prefix and by reserved", async ({ page, request }) => {
  const sku = `E2E-${uid()}`;
  await seedProduct(request, sku);
  await page.goto("/stock");
  await setStock(page, sku, 7);

  const row = page.getByRole("row").filter({ hasText: sku });
  await expect(row).toContainText("7");
  await expect(row).not.toContainText("No product");

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
  const sku = `E2E-${uid()}`;
  await page.goto(`/stock?prefix=${sku}&reserved=true`);
  await expect(page.getByLabel("SKU prefix")).toHaveValue(sku);
  await expect(page.getByRole("button", { name: "Clear" })).toBeVisible();
});

test("the SKU can only be chosen from the catalog's products", async ({ page, request }) => {
  const sku = `E2E-PICK-${uid()}`;
  await seedProduct(request, sku);
  await page.goto("/stock");

  // Nothing chosen = nothing to send; the field is a choice among products, not free text.
  await expect(skuField(page)).toHaveJSProperty("tagName", "SELECT");
  await expect(setButton(page)).toBeDisabled();

  // A prefix nothing in the catalog starts with offers nothing to choose.
  await page.getByLabel("Find product").fill(`NOPE-${uid()}`);
  await page.getByRole("button", { name: "Find", exact: true }).click();
  await expect(page.getByText("No product starts with this prefix.")).toBeVisible();
  await expect(skuField(page).locator("option:not([value=''])")).toHaveCount(0);
  await expect(setButton(page)).toBeDisabled();

  await pickProduct(page, sku);
  await expect(setButton(page)).toBeEnabled();
});

test("the picker pages through the catalog, the list labels each row from its own SKU", async ({ page, request }) => {
  // More products than one ListProducts page (20): the last of them is only on page 2.
  const prefix = `E2E-PG-${uid()}-`;
  const skus = Array.from({ length: 22 }, (_, i) => `${prefix}${String(i).padStart(2, "0")}`);
  for (let i = 0; i < skus.length; i += 6) await Promise.all(skus.slice(i, i + 6).map((s) => seedProduct(request, s)));
  const last = skus[skus.length - 1];
  const orphan = `${prefix}ORPHAN`; // sorts after the digits, so it is not a product either way
  await setStockViaAPI(request, last, 3);

  await page.goto("/stock");
  await page.getByLabel("Find product").fill(prefix);
  await page.getByRole("button", { name: "Find", exact: true }).click();
  const options = skuField(page).locator("option:not([value=''])");
  await expect(options).toHaveCount(20);
  await expect(skuField(page).locator(`option[value='${last}']`)).toHaveCount(0);
  await page.getByRole("button", { name: "Load more products" }).click();
  await expect(options).toHaveCount(22);
  await skuField(page).selectOption({ value: last });
  await expect(setButton(page)).toBeEnabled();
  await expect(page.getByRole("button", { name: "Load more products" })).toHaveCount(0);

  // The list: `last` is a product that is not on the picker's first page, `orphan` is no product.
  await showStocks(page, [last, orphan]);
  await page.goto(`/stock?prefix=${prefix}`);
  const orphanRow = page.getByRole("row").filter({ hasText: orphan });
  await expect(orphanRow).toContainText("No product");
  const lastRow = page.getByRole("row").filter({ hasText: last });
  await expect(lastRow).toHaveCount(1);
  await expect(lastRow).not.toContainText("No product");
  await expect(lastRow).not.toContainText("Deactivated");
  await expect(lastRow).not.toContainText("Status unknown");
});

test("a deactivated product is flagged in the list and can still be chosen", async ({ page, request }) => {
  const sku = `E2E-OFF-${uid()}`;
  await seedProduct(request, sku);
  await setStockViaAPI(request, sku, 2);
  await deactivateProduct(request, sku);

  await page.goto(`/stock?prefix=${sku}`);
  const row = page.getByRole("row").filter({ hasText: sku });
  await expect(row).toContainText("Deactivated");
  await expect(row).not.toContainText("No product");

  await pickProduct(page, sku);
  await expect(skuField(page).locator(`option[value='${sku}']`)).toContainText("deactivated");
  await expect(page.getByText("This product is deactivated; its stock can still be set.")).toBeVisible();
  await page.getByLabel("On hand").fill("9");
  await setButton(page).click();
  await expect(row).toContainText("9");
});

test("Edit on a SKU with no product says so and does not send it", async ({ page }) => {
  const orphan = `E2E-ORPHAN-${uid()}`;
  await showStocks(page, [orphan]);
  let sent = 0;
  await page.route("**/inventory.v1.InventoryService/SetStock", (route) => {
    sent++;
    return route.continue();
  });

  await page.goto(`/stock?prefix=${orphan}`);
  const row = page.getByRole("row").filter({ hasText: orphan });
  await expect(row).toContainText("No product");
  await row.getByRole("button", { name: "Edit" }).click();

  await expect(skuField(page)).toHaveValue(orphan);
  await expect(page.getByText(`${orphan} has no product in the catalog; save the product before stocking it.`)).toBeVisible();
  await expect(setButton(page)).toBeDisabled();
  expect(sent).toBe(0);
});

test("Edit on a deactivated SKU outside the picker's pages loads it, marked", async ({ page, request }) => {
  const sku = `E2E-EDIT-${uid()}`;
  await seedProduct(request, sku);
  await setStockViaAPI(request, sku, 5);
  await deactivateProduct(request, sku);

  await page.goto(`/stock?prefix=${sku}`);
  await page.getByRole("row").filter({ hasText: sku }).getByRole("button", { name: "Edit" }).click();
  // No Find was used: the option comes from the row, not from a page of products.
  await expect(skuField(page)).toHaveValue(sku);
  await expect(skuField(page).locator(`option[value='${sku}']`)).toContainText("deactivated");
  await expect(setButton(page)).toBeEnabled();
});

test("when the catalog cannot be read the list says unknown, never no product", async ({ page, request }) => {
  const sku = `E2E-DOWN-${uid()}`;
  await seedProduct(request, sku);
  await setStockViaAPI(request, sku, 1);
  await failing(page, "catalog.v1.CatalogService/QuotePrices");

  await page.goto(`/stock?prefix=${sku}`);
  const row = page.getByRole("row").filter({ hasText: sku });
  await expect(row).toContainText("Status unknown");
  await expect(row).not.toContainText("No product");
  await expect(row).not.toContainText("Deactivated");

  // The form cannot vouch for a SKU either, so it does not send it.
  await row.getByRole("button", { name: "Edit" }).click();
  await expect(page.getByText(`Could not check ${sku} against the catalog.`)).toBeVisible();
  await expect(setButton(page)).toBeDisabled();
});

test("when the product list cannot be read the picker says so and offers nothing", async ({ page }) => {
  await failing(page, "catalog.v1.CatalogService/ListProducts");
  await page.goto("/stock");
  await expect(page.getByRole("alert")).toContainText("catalog is down");
  await expect(skuField(page).locator("option:not([value=''])")).toHaveCount(0);
  await expect(setButton(page)).toBeDisabled();
});
