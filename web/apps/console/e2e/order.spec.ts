import { expect, test } from "@playwright/test";
import { withDB } from "./mode";

// /orders/$id: the id cell of the list links here; the page shows every line,
// the total, the reservation, and cancels. A deep link to an unknown id says so.

test("the list links to the order; the page shows lines, cancels, and polls the status", async ({
  page,
}) => {
  const customer = `e2e-detail-${Date.now()}`;
  await page.goto("/orders");
  await page.getByLabel("Customer").first().fill(customer);
  await page.getByRole("button", { name: "Create order" }).click();
  await page.getByLabel("Customer").nth(1).fill(customer);
  await page.getByRole("button", { name: "Filter" }).click();

  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row).toHaveCount(1);
  await row.getByRole("link", { name: /^[0-9a-f]{8}$/ }).click();
  await expect(page).toHaveURL(/\/orders\/[^/?]+$/);
  const id = new URL(page.url()).pathname.split("/").pop()!;
  await expect(
    page.locator('[data-slot="card-title"]', { hasText: id })
  ).toBeVisible();
  await expect(page.getByText(`customer ${customer}`)).toBeVisible();

  const lines = page.getByRole("row").filter({ hasText: "SKU-1" });
  await expect(lines).toHaveCount(1);
  await expect(lines).toContainText("12.99 USD"); // unit
  await expect(lines).toContainText("25.98 USD"); // 2 × 1299 minor
  await expect(page.getByTestId("order-total")).toContainText("25.98 USD");
  await expect(page.getByTestId("order-status")).toHaveText("PENDING");
  // Without a worker nothing reserves; with one the card flips on its own
  // (reservation.spec.ts covers the RESERVED path end to end).
  if (!withDB) await expect(page.getByText("none yet")).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Open in Reservations" })
  ).toHaveAttribute("href", `/reservations?order=${id}`);

  await page.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByTestId("order-status")).toHaveText("CANCELLED");
  await expect(page.getByRole("button", { name: "Cancel" })).toHaveCount(0);

  // Back on the list the row agrees (same cache key prefix, invalidated).
  await page.getByRole("link", { name: "All orders" }).click();
  await expect(page).toHaveURL(/\/orders$/);
  await page.getByLabel("Customer").nth(1).fill(customer);
  await page.getByRole("button", { name: "Filter" }).click();
  await expect(row.getByText("CANCELLED")).toBeVisible();
});

test("an unknown id is a not-found page, not an error", async ({ page }) => {
  await page.goto("/orders/order-that-never-existed");
  await expect(
    page.locator('[data-slot="card-title"]', {
      hasText: "order-that-never-existed",
    })
  ).toBeVisible();
  await expect(page.getByText("No such order")).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByRole("link", { name: "All orders" }).click();
  await expect(page).toHaveURL(/\/orders$/);
});
