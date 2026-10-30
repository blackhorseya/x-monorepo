import { expect, test, type Page } from "@playwright/test";
import { withDB } from "./mode";
import { seedProduct } from "./catalog";
import { uid } from "./uid";
import { confirm } from "./orders-ui";

// Request budget of the orders list. The server only has unary RPCs, so the console polls;
// this pins down what is allowed to poll. The list never calls GetReservation: the rows still
// waiting for something share one BatchGetReservations per tick, terminal rows (CANCELLED with
// its tombstone, nothing to wait for) are not asked about at all, and later pages must not be
// refetched every tick, only the first one (new / changed orders show up there). In-memory
// mode: no worker, so a PENDING order keeps waiting and the reservation answers are scripted
// where a test needs them.
test.skip(withDB, "a worker reserves these orders, so reservations are not the scripted ones (reservation.spec.ts covers that path)");

const WINDOW_MS = 8_000;
const PAGE = 20;
const TOTAL = PAGE + 5; // two pages

const post = (page: Page, method: string, body: object) =>
  page.request.post(`/api/${method}`, { data: body });

test("terminal rows and later pages stop polling", async ({ page }) => {
  const customer = `e2e-poll-${uid()}`;
  for (let i = 0; i < TOTAL; i++) {
    const id = await createOrder(page, customer);
    await post(page, "ordering.v1.OrderService/CancelOrder", { id });
  }

  // What a worker leaves behind for a cancelled order: a CANCELLED tombstone, the final state.
  await page.route("**/inventory.v1.InventoryService/BatchGetReservations", (route) => {
    const { orderIds } = route.request().postDataJSON() as { orderIds: string[] };
    return route.fulfill({
      json: { reservations: orderIds.map((orderId) => ({ orderId, status: "RESERVATION_STATUS_CANCELLED" })) },
    });
  });

  const calls = { reservation: 0, batch: 0, firstPage: 0, laterPages: 0 };
  let counting = false;
  page.on("request", (req) => {
    if (!counting || req.method() !== "POST") return;
    const url = req.url();
    if (url.endsWith("/inventory.v1.InventoryService/GetReservation")) calls.reservation++;
    else if (url.endsWith("/inventory.v1.InventoryService/BatchGetReservations")) calls.batch++;
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
  await expect(page.getByTitle("Open in Reservations")).toHaveCount(TOTAL);

  counting = true;
  await page.waitForTimeout(WINDOW_MS);
  counting = false;
  console.log(`requests in ${WINDOW_MS / 1000}s:`, JSON.stringify(calls));

  expect(calls.reservation).toBe(0);
  expect(calls.batch).toBe(0);
  expect(calls.laterPages).toBe(0);
  // The first page still refreshes, or a new order / a cancel from elsewhere would never show.
  expect(calls.firstPage).toBeGreaterThanOrEqual(2);
});

// PENDING rows are the ones that keep waiting (no worker here, so they never get a reservation).
// They must share one BatchGetReservations per tick — carrying every waiting row on the loaded
// pages — instead of a GetReservation each.
test("PENDING rows share one batch request per tick", async ({ page }) => {
  const customer = `e2e-pending-${uid()}`;
  for (let i = 0; i < TOTAL; i++) await createOrder(page, customer);

  const calls = { reservation: 0, batch: 0, firstPage: 0, laterPages: 0 };
  const batchSizes: number[] = [];
  let counting = false;
  page.on("request", (req) => {
    if (!counting || req.method() !== "POST") return;
    const url = req.url();
    if (url.endsWith("/inventory.v1.InventoryService/GetReservation")) calls.reservation++;
    else if (url.endsWith("/inventory.v1.InventoryService/BatchGetReservations")) {
      calls.batch++;
      batchSizes.push((req.postDataJSON() as { orderIds: string[] }).orderIds.length);
    } else if (url.endsWith("/ordering.v1.OrderService/ListOrders")) {
      const { pageToken } = req.postDataJSON() as { pageToken?: string };
      if (pageToken) calls.laterPages++;
      else calls.firstPage++;
    }
  });

  await page.goto(`/orders?customer=${customer}`);
  const rows = page.getByRole("row").filter({ hasText: customer });
  await expect(rows).toHaveCount(PAGE);
  await page.getByRole("button", { name: "Load more" }).click();
  await expect(rows).toHaveCount(TOTAL);
  await expect(page.getByText("none yet")).toHaveCount(TOTAL);

  counting = true;
  await page.waitForTimeout(WINDOW_MS);
  counting = false;
  console.log(`requests in ${WINDOW_MS / 1000}s:`, JSON.stringify({ ...calls, batchSizes }));

  expect(calls.reservation).toBe(0);
  expect(calls.laterPages).toBe(0);
  // One per 2s tick (4 in the window, one more if the window straddles a tick), every waiting row in it.
  expect(calls.batch).toBeGreaterThanOrEqual(2);
  expect(calls.batch).toBeLessThanOrEqual(5);
  expect(batchSizes.every((n) => n === TOTAL)).toBe(true);
});

// The price is the catalog's, so the SKU has to be a product: one per customer, seeded on first use.
const seeded = new Set<string>();
const createOrder = async (page: Page, customer: string) => {
  const sku = `E2E-P-${customer}`;
  if (!seeded.has(sku)) {
    await seedProduct(page.request, sku, { amountMinor: 100 });
    seeded.add(sku);
  }
  const res = await post(page, "ordering.v1.OrderService/CreateOrder", {
    customerId: customer,
    items: [{ sku, quantity: 1 }],
  });
  return ((await res.json()) as { order: { id: string } }).order.id;
};

// Inventory accepts OrderCancelled before OrderCreated (a stopped or lagging worker) and then
// records a CANCELLED tombstone, so "cancelled order, no reservation yet" is not a final state:
// the row must keep looking until the reservation shows up.
test("a cancelled order picks up a reservation that appears late", async ({ page }) => {
  const customer = `e2e-late-${uid()}`;
  const id = await createOrder(page, customer);
  await post(page, "ordering.v1.OrderService/CancelOrder", { id });

  // The worker "comes back" after the list has loaded: NotFound until then, then the tombstone.
  let late = false;
  await page.route("**/inventory.v1.InventoryService/BatchGetReservations", async (route) => {
    if (!late) return route.fulfill({ json: {} }); // no reservation for anyone yet
    return route.fulfill({ json: { reservations: [{ orderId: id, status: "RESERVATION_STATUS_CANCELLED" }] } });
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
  const customer = `e2e-race-${uid()}`;
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
  await confirm(page, "Cancel order");
  await expect(row.getByText("CANCELLED")).toBeVisible();
  release();
  await page.waitForTimeout(300); // far below the 2s tick: only the stale response can act now
  // A snapshot, not a retrying expect: the next poll tick would repair a regression and hide it.
  const text = await row.innerText();
  expect(text).toContain("CANCELLED");
  expect(text).not.toContain("PENDING");
  expect(await row.getByRole("button", { name: "Cancel" }).count()).toBe(0);
});

// The wait for a late reservation starts when the order turns terminal, not when the query was
// created: a PENDING order that was fetched many times (every list / detail mount fetches) must
// still get its full window after it is cancelled.
test("a cancelled order still waits for its reservation after many fetches while PENDING", async ({ page }) => {
  const customer = `e2e-budget-${uid()}`;
  const id = await createOrder(page, customer);

  let late = false;
  let fetches = 0;
  await page.route("**/inventory.v1.InventoryService/BatchGetReservations", async (route) => {
    fetches++;
    if (!late) return route.fulfill({ json: {} });
    return route.fulfill({ json: { reservations: [{ orderId: id, status: "RESERVATION_STATUS_CANCELLED" }] } });
  });

  await page.goto(`/orders?customer=${customer}`);
  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row.getByText("none yet")).toBeVisible();
  // Every mount of the list and of the detail page fetches the reservation again.
  while (fetches < 20) {
    await row.getByRole("link", { name: /^[0-9a-f]{8}$/ }).click();
    await expect(page.getByRole("link", { name: "All orders" })).toBeVisible();
    await page.goBack();
    await expect(row.getByText("none yet")).toBeVisible();
  }

  await row.getByRole("button", { name: "Cancel" }).click();
  await confirm(page, "Cancel order");
  await expect(row.getByText("CANCELLED")).toBeVisible();
  late = true; // the worker comes back
  await expect(row.getByTitle("Open in Reservations")).toHaveText("CANCELLED", { timeout: 8_000 });
});

// Once the wait for a reservation that is not there yet has run out (30s after the order turned
// terminal) the row stops asking. Coming back to the tab must still look once more, as a
// refocused page always has: the worker may have caught up while nobody was watching.
test("returning to the tab looks again at a row whose wait ran out", async ({ page }) => {
  await page.clock.install();
  const customer = `e2e-focus-${uid()}`;
  const id = await createOrder(page, customer);
  await post(page, "ordering.v1.OrderService/CancelOrder", { id });

  let late = false;
  let fetches = 0;
  await page.route("**/inventory.v1.InventoryService/BatchGetReservations", async (route) => {
    fetches++;
    if (!late) return route.fulfill({ json: {} });
    return route.fulfill({ json: { reservations: [{ orderId: id, status: "RESERVATION_STATUS_CANCELLED" }] } });
  });

  await page.goto(`/orders?customer=${customer}`);
  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row.getByText("none yet")).toBeVisible();

  // Let the 30s window run out and the row go quiet, then the worker "comes back".
  await page.clock.fastForward(40_000);
  await page.waitForTimeout(500);
  const quiet = fetches;
  await page.waitForTimeout(2_500);
  expect(fetches).toBe(quiet);
  late = true;

  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await expect(row.getByTitle("Open in Reservations")).toHaveText("CANCELLED", { timeout: 5_000 });
});

// The same, but a poll for another row is in flight when the tab comes back: the refresh of every
// row must wait its turn, not be dropped because a request is already running.
test("returning to the tab while another row's request is in flight still looks at the expired row", async ({ page }) => {
  await page.clock.install();
  const customer = `e2e-busy-${uid()}`;
  const expired = await createOrder(page, customer); // cancelled: its wait for a reservation runs out
  await post(page, "ordering.v1.OrderService/CancelOrder", { id: expired });
  await createOrder(page, customer); // PENDING: keeps polling, alone once the other one went quiet

  let late = false;
  let hold = false;
  let held!: () => void;
  const captured = new Promise<void>((r) => (held = r));
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  await page.route("**/inventory.v1.InventoryService/BatchGetReservations", async (route) => {
    const { orderIds } = route.request().postDataJSON() as { orderIds: string[] };
    if (hold && !orderIds.includes(expired)) {
      hold = false; // only the first poll that leaves the expired row out is delayed
      held();
      await released;
      return route.fulfill({ json: {} });
    }
    if (!late || !orderIds.includes(expired)) return route.fulfill({ json: {} });
    return route.fulfill({ json: { reservations: [{ orderId: expired, status: "RESERVATION_STATUS_CANCELLED" }] } });
  });

  await page.goto(`/orders?customer=${customer}`);
  const row = page.getByRole("row").filter({ has: page.getByRole("link", { name: expired.slice(0, 8) }) });
  await expect(row.getByText("none yet")).toBeVisible();

  await page.clock.fastForward(40_000); // the expired row's wait is over
  hold = true;
  await captured; // a poll for the PENDING row alone is now in flight
  late = true; // the worker comes back
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  release();
  await expect(row.getByTitle("Open in Reservations")).toHaveText("CANCELLED", { timeout: 5_000 });
});

// A refocus that arrives while a request is in flight is queued (once); if the tab is hidden when
// that request ends the queue stays, and the next refocus must consume it rather than run its own
// full refresh and then the queued one on top: two full refreshes in total, not three.
test("a queued refocus is consumed by the next full refresh", async ({ page }) => {
  const customer = `e2e-queue-${uid()}`;
  const id = await createOrder(page, customer);
  await post(page, "ordering.v1.OrderService/CancelOrder", { id });

  let calls = 0;
  let hold = false;
  let held!: () => void;
  let release!: () => void;
  await page.route("**/inventory.v1.InventoryService/BatchGetReservations", async (route) => {
    calls++;
    if (hold) {
      hold = false;
      held();
      await new Promise<void>((r) => (release = r));
    }
    return route.fulfill({ json: { reservations: [{ orderId: id, status: "RESERVATION_STATUS_CANCELLED" }] } });
  });
  const visibility = (state: "visible" | "hidden") =>
    page.evaluate((s) => {
      Object.defineProperty(document, "visibilityState", { configurable: true, get: () => s });
      document.dispatchEvent(new Event("visibilitychange", { bubbles: true }));
    }, state);

  await page.goto(`/orders?customer=${customer}`);
  await expect(page.getByTitle("Open in Reservations")).toHaveText("CANCELLED");
  await page.waitForTimeout(200);
  calls = 0;

  const captured = new Promise<void>((r) => (held = r));
  hold = true;
  await visibility("visible"); // full refresh #1, held in flight
  await captured;
  await visibility("visible"); // queued behind it
  await visibility("hidden");
  release();
  await page.waitForTimeout(2_500);
  expect(calls).toBe(1); // hidden: the queue does not start anything

  await visibility("visible"); // full refresh #2 — and the queue it consumes
  await page.waitForTimeout(1_500);
  expect(calls).toBe(2);
  await page.waitForTimeout(4_200);
  expect(calls).toBe(2); // nothing keeps polling
});
