import { expect, test } from "@playwright/test";

// The server keeps orders in memory for the whole run and the tests run in
// parallel, so each one works under its own customer id and filters by it.
const customer = (t: string) => `e2e-${t}-${Date.now()}`;

test("/ lands on /orders", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/\/orders$/);
  await expect(page.locator('[data-slot="card-title"]', { hasText: "Orders" })).toBeVisible();
});

test("create, see, cancel an order", async ({ page }) => {
  const id = customer("cancel");
  await page.goto("/orders");
  await page.getByLabel("Customer").first().fill(id);
  await page.getByRole("button", { name: "Create order" }).click();

  await page.getByLabel("Customer").nth(1).fill(id);
  await page.getByRole("button", { name: "Filter" }).click();
  await expect(page).toHaveURL(new RegExp(`customer=${id}`));

  const row = page.getByRole("row").filter({ hasText: id });
  await expect(row).toHaveCount(1);
  await expect(row.getByText("PENDING")).toBeVisible();
  await expect(row.getByText("25.98 USD")).toBeVisible(); // 2 × 1299 minor USD
  await expect(row.getByText("none yet")).toBeVisible(); // no worker: no reservation

  await row.getByRole("button", { name: "Cancel" }).click();
  await expect(row.getByText("CANCELLED")).toBeVisible();
  await expect(row.getByRole("button", { name: "Cancel" })).toHaveCount(0);

  // The status filter is a search param and the list follows it.
  await page.getByRole("button", { name: "PENDING", exact: true }).click();
  await expect(page).toHaveURL(/status=PENDING/);
  await expect(page.getByText(`No orders with status PENDING for ${id} yet.`)).toBeVisible();
});

test("validation failures surface the field", async ({ page }) => {
  await page.goto("/orders");
  await page.getByLabel("Customer").first().fill(customer("invalid"));
  await page.getByLabel("Qty").fill("0");
  await page.getByRole("button", { name: "Create order" }).click();
  const alert = page.getByRole("alert");
  await expect(alert).toContainText("Request failed");
  await expect(alert).toContainText("request.validation_failed");
  await expect(alert).toContainText("items[0].quantity");
});
