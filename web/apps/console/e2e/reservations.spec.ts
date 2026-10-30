import { expect, test } from "@playwright/test";
import { withDB } from "./mode";
import { seedProduct } from "./catalog";
import { uid } from "./uid";
import { confirm, createCard, filterCard } from "./orders-ui";

// /reservations: the status filter buttons drive ?status=, ?order= pins one
// reservation above the list. Works in both modes — in-memory just has nothing
// to show, because nobody consumes OrderCreated without a worker.

const title = (page: import("@playwright/test").Page, text: string) =>
  page.locator('[data-slot="card-title"]', { hasText: text });

test("status filter buttons drive the URL", async ({ page }) => {
  await page.goto("/reservations");
  await expect(title(page, "Reservations")).toBeVisible();
  await expect(
    page.getByRole("button", { name: "ANY", exact: true })
  ).toBeVisible();

  await page.getByRole("button", { name: "REJECTED", exact: true }).click();
  await expect(page).toHaveURL(/status=REJECTED/);
  // In-memory nothing is ever reserved, let alone rejected; a shared Postgres
  // (local dev, k6) may hold anything, so only the URL is asserted there.
  if (!withDB)
    await expect(
      page.getByText("No reservations with status REJECTED yet.")
    ).toBeVisible();

  await page.getByRole("button", { name: "ANY", exact: true }).click();
  await expect(page).not.toHaveURL(/status=/);
});

test("?order= pins the order even when there is no reservation", async ({
  page,
}) => {
  const orderId = "order-that-never-existed";
  await page.goto(`/reservations?order=${orderId}`);
  await expect(title(page, orderId)).toBeVisible();
  await expect(
    page.getByText(
      "No reservation yet — the worker has not consumed this order."
    )
  ).toBeVisible();

  await page.getByRole("link", { name: "Clear" }).click();
  await expect(page).not.toHaveURL(/order=/);
  await expect(title(page, orderId)).toHaveCount(0);
});

test.describe("with a worker", () => {
  test.skip(!withDB, "needs Postgres + worker (E2E_DATABASE_URL)");
  const WORKER = { timeout: 30_000 };

  test("the list filters by status and the pinned card follows the order", async ({
    page,
    request,
  }) => {
    const sku = `E2E-R-${uid()}`;
    const customer = `e2e-res-${uid()}`;
    await seedProduct(request, sku);

    await page.goto(`/stock?prefix=${sku}`); // paged by SKU: filter, or older E2E-* rows push ours off page 1
    await page.getByLabel("SKU", { exact: true }).fill(sku);
    await page.getByLabel("On hand").fill("3");
    await page.getByRole("button", { name: "Set", exact: true }).click();
    await expect(page.getByRole("row").filter({ hasText: sku })).toHaveCount(1);

    await page.goto("/orders");
    await createCard(page).getByLabel("Customer").fill(customer);
    await createCard(page).getByLabel("SKU").fill(sku);
    await page.getByLabel("Qty").fill("1");
    await page.getByRole("button", { name: "Create order" }).click();
    await filterCard(page).getByLabel("Customer").fill(customer);
    await page.getByRole("button", { name: "Filter" }).click();
    const orderRow = page.getByRole("row").filter({ hasText: customer });
    await expect(orderRow.getByText("RESERVED")).toBeVisible(WORKER);
    // The badge's link carries the full order id.
    const href = await orderRow
      .getByRole("link", { name: "RESERVED" })
      .getAttribute("href");
    const orderId = new URL(href ?? "", "http://x").searchParams.get("order");
    expect(orderId).toBeTruthy();

    // RESERVED list has it, RELEASED list does not. Scoped to the list card:
    // with ?order= the pinned card shows the same row again.
    const list = page.locator('[data-slot="card"]', {
      has: title(page, "Reservations"),
    });
    await page.goto("/reservations?status=RESERVED");
    const row = list.getByRole("row").filter({ hasText: orderId! });
    await expect(row).toHaveCount(1);
    await expect(row).toContainText(`${sku} × 1`);
    await page.getByRole("button", { name: "RELEASED", exact: true }).click();
    await expect(page).toHaveURL(/status=RELEASED/);
    await expect(row).toHaveCount(0);

    // Pinned card, then cancel the order: the pin follows to RELEASED.
    await page.goto(`/reservations?order=${orderId}`);
    await expect(title(page, orderId!)).toBeVisible();
    const pinned = page.locator('[data-slot="card"]').first();
    await expect(pinned.getByText("RESERVED")).toBeVisible();

    await page.goto(`/orders?customer=${customer}`);
    await orderRow.getByRole("button", { name: "Cancel" }).click();
    await confirm(page, "Cancel order");
    await expect(orderRow.getByText("CANCELLED")).toBeVisible();

    await page.goto(`/reservations?order=${orderId}&status=RELEASED`);
    await expect(pinned.getByText("RELEASED")).toBeVisible(WORKER);
    await expect(row).toHaveCount(1);
  });
});
