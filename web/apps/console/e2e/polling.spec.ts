import { expect, test, type Page } from "@playwright/test";
import { withDB } from "./mode";

// Request budget of the orders list. The server only has unary RPCs, so the console polls;
// this pins down what is allowed to poll. Terminal rows (CANCELLED, no reservation to wait
// for) must not poll GetReservation, and later pages must not be refetched every tick, only
// the first one (new / changed orders show up there). In-memory mode is enough: no worker,
// so a PENDING order keeps "waiting", a cancelled one is terminal.
test.skip(withDB, "a worker reserves these orders, so they are not 'none yet' rows (reservation.spec.ts covers that path)");

const WINDOW_MS = 8_000;
const PAGE = 20;
const TOTAL = PAGE + 5; // two pages

const post = (page: Page, method: string, body: object) =>
  page.request.post(`/api/${method}`, { data: body });

test("terminal rows and later pages stop polling", async ({ page }) => {
  const customer = `e2e-poll-${Date.now()}`;
  for (let i = 0; i < TOTAL; i++) {
    const res = await post(page, "ordering.v1.OrderService/CreateOrder", {
      customerId: customer,
      items: [{ sku: "SKU-1", quantity: 1, unitPrice: { amountMinor: "100", currency: "USD" } }],
    });
    const { order } = (await res.json()) as { order: { id: string } };
    await post(page, "ordering.v1.OrderService/CancelOrder", { id: order.id });
  }

  const calls = { reservation: 0, firstPage: 0, laterPages: 0 };
  let counting = false;
  page.on("request", (req) => {
    if (!counting || req.method() !== "POST") return;
    const url = req.url();
    if (url.endsWith("/inventory.v1.InventoryService/GetReservation")) calls.reservation++;
    else if (url.endsWith("/ordering.v1.OrderService/ListOrders")) {
      const { pageToken } = req.postDataJSON() as { pageToken?: string };
      if (pageToken) calls.laterPages++;
      else calls.firstPage++;
    }
  });

  await page.goto(`/orders?customer=${customer}`);
  await expect(page.getByRole("row").filter({ hasText: customer })).toHaveCount(PAGE);
  await page.getByRole("button", { name: "Load more" }).click();
  await expect(page.getByRole("row").filter({ hasText: customer })).toHaveCount(TOTAL);
  await expect(page.getByText("none yet")).toHaveCount(TOTAL);

  counting = true;
  await page.waitForTimeout(WINDOW_MS);
  counting = false;
  console.log(`requests in ${WINDOW_MS / 1000}s:`, JSON.stringify(calls));

  expect(calls.reservation).toBe(0);
  expect(calls.laterPages).toBe(0);
  // The first page still refreshes, or a new order / a cancel from elsewhere would never show.
  expect(calls.firstPage).toBeGreaterThanOrEqual(2);
});
