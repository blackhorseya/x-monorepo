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

const createOrder = async (page: Page, customer: string) => {
  const res = await post(page, "ordering.v1.OrderService/CreateOrder", {
    customerId: customer,
    items: [{ sku: "SKU-1", quantity: 1, unitPrice: { amountMinor: "100", currency: "USD" } }],
  });
  return ((await res.json()) as { order: { id: string } }).order.id;
};

// Inventory accepts OrderCancelled before OrderCreated (a stopped or lagging worker) and then
// records a CANCELLED tombstone, so "cancelled order, no reservation yet" is not a final state:
// the row must keep looking until the reservation shows up.
test("a cancelled order picks up a reservation that appears late", async ({ page }) => {
  const customer = `e2e-late-${Date.now()}`;
  const id = await createOrder(page, customer);
  await post(page, "ordering.v1.OrderService/CancelOrder", { id });

  // The worker "comes back" after the list has loaded: NotFound until then, then the tombstone.
  let late = false;
  await page.route("**/inventory.v1.InventoryService/GetReservation", async (route) => {
    if (!late) return route.fulfill({ status: 404, json: { code: "not_found", message: "reservation not found" } });
    return route.fulfill({ json: { reservation: { orderId: id, status: "RESERVATION_STATUS_CANCELLED" } } });
  });

  await page.goto(`/orders?customer=${customer}`);
  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row.getByText("none yet")).toBeVisible();
  late = true;
  await expect(row.getByTitle("Open in Reservations")).toHaveText("CANCELLED", { timeout: 8_000 });
});

// A first-page poll answered after a mutation has refreshed the list holds the older state; it
// must not overwrite the cancel the user just made.
test("a late poll response does not undo a cancel", async ({ page }) => {
  const customer = `e2e-race-${Date.now()}`;
  await createOrder(page, customer);

  let hold = false;
  let held!: () => void;
  const captured = new Promise<void>((r) => (held = r));
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  await page.route("**/ordering.v1.OrderService/ListOrders", async (route) => {
    if (!hold) return route.continue();
    hold = false; // only the first poll after this point is delayed
    const res = await route.fetch(); // answered now, with the order still PENDING ...
    held();
    await released; // ... but delivered to the browser late
    await route.fulfill({ response: res });
  });

  await page.goto(`/orders?customer=${customer}`);
  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row.getByText("PENDING")).toBeVisible();

  hold = true;
  await captured;
  await row.getByRole("button", { name: "Cancel" }).click();
  await expect(row.getByText("CANCELLED")).toBeVisible();
  release();
  await page.waitForTimeout(300); // far below the 2s tick: only the stale response can act now
  // A snapshot, not a retrying expect: the next poll tick would repair a regression and hide it.
  const text = await row.innerText();
  expect(text).toContain("CANCELLED");
  expect(text).not.toContain("PENDING");
  expect(await row.getByRole("button", { name: "Cancel" }).count()).toBe(0);
});
