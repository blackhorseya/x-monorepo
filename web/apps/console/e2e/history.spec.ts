import { expect, test, type Page } from "@playwright/test";
import { withDB } from "./mode";
import { seedProduct } from "./catalog";
import { uid } from "./uid";
import { confirm, createCard, createOrder } from "./orders-ui";
import { setStock } from "./stock-ui";
import { expectReservationStatus } from "./reservation-ui";

// The History card of /orders/$id (#427 slice 3): the order's transitions as a timeline, oldest first, from
// ordering.v1.OrderService/GetOrderHistory. Real server first (a worker in DB mode), then the shapes the server
// cannot be made to produce on demand (an empty history, a cause or actor this build does not know, a failing
// call), scripted with page.route.

const GET_ORDER = "**/ordering.v1.OrderService/GetOrder";
const GET_HISTORY = "**/ordering.v1.OrderService/GetOrderHistory";
const NO_RESERVATION = "**/inventory.v1.InventoryService/GetReservation";

const rows = (page: Page) => page.getByTestId("history-row");
const texts = async (page: Page, testid: string) => page.getByTestId(testid).allTextContents();

test("a completed order's timeline lists every transition in order", async ({ page, request }) => {
  test.skip(!withDB, "needs the worker (DB mode) to confirm the order");
  const sku = `E2E-H-${uid()}`;
  await seedProduct(request, sku);
  await page.goto(`/stock?prefix=${sku}`);
  await setStock(page, sku, 5);
  await page.goto("/orders");
  await createCard(page).getByLabel("Customer").fill(`e2e-hist-${uid()}`);
  await createCard(page).getByLabel("SKU").fill(sku);
  const id = await createOrder(page);

  await page.goto(`/orders/${id}`);
  const reservation = page.locator('[data-slot="card"]', { hasText: "Reservation" }).last();
  await expectReservationStatus(reservation, "RESERVED", { timeout: 30_000 });
  await expect(page.getByTestId("order-status")).toHaveText("CONFIRMED", { timeout: 30_000 });
  await page.getByRole("button", { name: "Complete", exact: true }).click();
  await confirm(page, "Complete order");
  await expect(page.getByTestId("order-status")).toHaveText("COMPLETED");

  const card = page.getByTestId("history-card");
  await expect(card).toBeVisible();
  await expect(rows(page)).toHaveCount(3, { timeout: 15_000 });
  expect(await texts(page, "history-event")).toEqual(["CREATED", "CONFIRMED", "COMPLETED"]);
  expect(await texts(page, "history-transition")).toEqual(["PENDING", "PENDING → CONFIRMED", "CONFIRMED → COMPLETED"]);
  expect(await texts(page, "history-actor")).toEqual(["Anonymous", "System", "Anonymous"]); // auth off: no principal on the rpc calls
  expect(await texts(page, "history-cause")).toEqual(["Requested by a client", "Stock reserved", "Requested by a client"]);
  // auth.mode off: nobody is an admin, so no row names who acted, whatever the table holds.
  await expect(page.getByTestId("history-subject")).toHaveCount(0);
});

test("a cancelled order's timeline ends with the cancellation", async ({ page, request }) => {
  test.skip(withDB, "a worker would confirm the order before it can be cancelled in PENDING");
  const sku = `E2E-HC-${uid()}`;
  await seedProduct(request, sku);
  await page.goto("/orders");
  await createCard(page).getByLabel("Customer").fill(`e2e-histc-${uid()}`);
  await createCard(page).getByLabel("SKU").fill(sku);
  const id = await createOrder(page);

  await page.goto(`/orders/${id}`);
  await expect(rows(page)).toHaveCount(1);
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await confirm(page, "Cancel order");
  await expect(page.getByTestId("order-status")).toHaveText("CANCELLED");
  await expect(rows(page)).toHaveCount(2, { timeout: 15_000 }); // the cancel invalidates the card's query
  expect(await texts(page, "history-event")).toEqual(["CREATED", "CANCELLED"]);
  expect(await texts(page, "history-transition")).toEqual(["PENDING", "PENDING → CANCELLED"]);
});

// ---- scripted ------------------------------------------------------------------------------------------

const order = (id: string, status: string) => ({
  id,
  customerId: "someone",
  status,
  items: [],
  total: { amountMinor: "100", currency: "USD" },
});
const entry = (seq: number, from: string, to: string, extra: Record<string, unknown> = {}) => ({
  seq,
  fromStatus: from,
  toStatus: to,
  occurredTime: `2026-11-04T10:0${seq}:00Z`,
  actorKind: "ORDER_HISTORY_ACTOR_KIND_SYSTEM",
  cause: "ORDER_HISTORY_CAUSE_STOCK_RESERVED",
  ...extra,
});

async function scriptOrder(page: Page, id: string, status = "ORDER_STATUS_COMPLETED") {
  await page.route(GET_ORDER, (route) => route.fulfill({ json: { order: order(id, status) } }));
  await page.route(NO_RESERVATION, (route) => route.fulfill({ status: 404, json: { code: "not_found", message: "none" } }));
}

test("an order with no history shows an explicit empty state, not an error", async ({ page }) => {
  const id = `e2e-hist-empty-${uid()}`;
  await scriptOrder(page, id);
  await page.route(GET_HISTORY, (route) => route.fulfill({ json: {} }));
  await page.goto(`/orders/${id}`);
  await expect(page.getByTestId("history-card")).toBeVisible();
  await expect(page.getByTestId("history-empty")).toContainText("before order history was recorded");
  await expect(rows(page)).toHaveCount(0);
  await expect(page.getByTestId("history-error")).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
});

test("NOT_FOUND and any other failure are told apart", async ({ page }) => {
  const id = `e2e-hist-err-${uid()}`;
  await scriptOrder(page, id);
  let answer: "not_found" | "unavailable" = "not_found";
  await page.route(GET_HISTORY, (route) =>
    answer === "not_found"
      ? route.fulfill({ status: 404, headers: { "x-error-code": "order.not_found" }, json: { code: "not_found", message: "SECRET-1" } })
      : route.fulfill({ status: 503, headers: { "x-error-code": "x.unavailable" }, json: { code: "unavailable", message: "backend gone" } }),
  );
  await page.goto(`/orders/${id}`);
  await expect(page.getByTestId("history-not-found")).toBeVisible();
  await expect(page.getByTestId("history-error")).toHaveCount(0);
  await expect(page.getByTestId("history-empty")).toHaveCount(0);

  answer = "unavailable";
  await page.reload();
  await expect(page.getByTestId("history-error")).toContainText("backend gone");
  await expect(page.getByTestId("history-not-found")).toHaveCount(0);
  await expect(page.getByTestId("history-empty")).toHaveCount(0);
});

test("a cause or actor this build does not know degrades to a fixed label", async ({ page }) => {
  const id = `e2e-hist-unknown-${uid()}`;
  await scriptOrder(page, id, "ORDER_STATUS_CANCELLED");
  await page.route(GET_HISTORY, (route) =>
    route.fulfill({
      json: {
        entries: [
          entry(1, "ORDER_STATUS_UNSPECIFIED", "ORDER_STATUS_PENDING", { cause: "ORDER_HISTORY_CAUSE_RPC_REQUEST", actorKind: "ORDER_HISTORY_ACTOR_KIND_CUSTOMER" }),
          entry(2, "ORDER_STATUS_PENDING", "ORDER_STATUS_CANCELLED", { cause: "ORDER_HISTORY_CAUSE_UNSPECIFIED", actorKind: "ORDER_HISTORY_ACTOR_KIND_UNSPECIFIED" }),
          entry(3, "ORDER_STATUS_CANCELLED", "ORDER_STATUS_CANCELLED", { cause: 99, actorKind: 99 }), // numbers the build has no name for
          entry(4, "ORDER_STATUS_CANCELLED", "ORDER_STATUS_CANCELLED", { cause: "ORDER_HISTORY_CAUSE_STOCK_SHORTAGE", actorKind: "ORDER_HISTORY_ACTOR_KIND_SYSTEM" }),
          entry(5, "ORDER_STATUS_CANCELLED", "ORDER_STATUS_CANCELLED", { cause: "ORDER_HISTORY_CAUSE_RESERVATION_EXPIRED", actorKind: "ORDER_HISTORY_ACTOR_KIND_SYSTEM" }),
        ],
      },
    }),
  );
  await page.goto(`/orders/${id}`);
  await expect(rows(page)).toHaveCount(5);
  expect(await texts(page, "history-cause")).toEqual([
    "Requested by a client",
    "Unknown cause",
    "Unknown cause",
    "Not enough stock",
    "Reservation expired",
  ]);
  expect(await texts(page, "history-actor")).toEqual(["Customer", "Unknown", "Unknown", "System", "System"]);
  await expect(page.getByRole("alert")).toHaveCount(0);
});

test("the timeline follows the order: a new transition appears, and polling stops once the order is final", async ({ page }) => {
  const id = `e2e-hist-poll-${uid()}`;
  const steps = [
    { status: "ORDER_STATUS_PENDING", entries: [entry(1, "ORDER_STATUS_UNSPECIFIED", "ORDER_STATUS_PENDING")] },
    {
      status: "ORDER_STATUS_CONFIRMED",
      entries: [entry(1, "ORDER_STATUS_UNSPECIFIED", "ORDER_STATUS_PENDING"), entry(2, "ORDER_STATUS_PENDING", "ORDER_STATUS_CONFIRMED")],
    },
    {
      status: "ORDER_STATUS_COMPLETED",
      entries: [
        entry(1, "ORDER_STATUS_UNSPECIFIED", "ORDER_STATUS_PENDING"),
        entry(2, "ORDER_STATUS_PENDING", "ORDER_STATUS_CONFIRMED"),
        entry(3, "ORDER_STATUS_CONFIRMED", "ORDER_STATUS_COMPLETED"),
      ],
    },
  ];
  let step = 0;
  let historyCalls = 0;
  await page.route(GET_ORDER, (route) => route.fulfill({ json: { order: order(id, steps[step].status) } }));
  await page.route(NO_RESERVATION, (route) => route.fulfill({ status: 404, json: { code: "not_found", message: "none" } }));
  await page.route(GET_HISTORY, (route) => {
    historyCalls++;
    return route.fulfill({ json: { entries: steps[step].entries } });
  });

  await page.goto(`/orders/${id}`);
  await expect(rows(page)).toHaveCount(1);
  step = 1;
  await expect(rows(page)).toHaveCount(2, { timeout: 10_000 }); // the order's own poll sees CONFIRMED; the card follows
  step = 2;
  await expect(page.getByTestId("order-status")).toHaveText("COMPLETED", { timeout: 20_000 });
  await expect(rows(page)).toHaveCount(3, { timeout: 10_000 });

  // Final order: the card no longer polls. (A snapshot over more than two poll periods of the fast pace.)
  await page.waitForTimeout(500);
  const settled = historyCalls;
  await page.waitForTimeout(4_500);
  expect(historyCalls).toBe(settled);
});

// The order can reach its final state while the card's first answer is still on the way (a slow network, a busy
// server): that first answer is a snapshot from before the last transition. The card must not stop there — a final
// order is not polled, so nothing but a catch-up fetch would ever bring the last row in (#443). Every wait below is
// a request or a reply the test itself controls or observes; the one timed window (the final order is not asked
// again) is longer than the order's own 2s poll period.
for (const final of ["COMPLETED", "CANCELLED"] as const) {
  test(`a first answer still in flight when the order turns ${final} does not leave the timeline short`, async ({ page }) => {
    const id = `e2e-hist-slow-${final}-${uid()}`;
    const finalStatus = `ORDER_STATUS_${final}`;
    const created = [entry(1, "ORDER_STATUS_UNSPECIFIED", "ORDER_STATUS_PENDING")];
    const full = [...created, entry(2, "ORDER_STATUS_PENDING", finalStatus, { cause: "ORDER_HISTORY_CAUSE_RPC_REQUEST" })];
    let status = "ORDER_STATUS_PENDING";
    let entries = created;
    await page.route(GET_ORDER, (route) => route.fulfill({ json: { order: order(id, status) } }));
    await page.route(NO_RESERVATION, (route) => route.fulfill({ status: 404, json: { code: "not_found", message: "none" } }));

    let firstArrived!: () => void;
    const firstIsHeld = new Promise<void>((r) => (firstArrived = r));
    let release!: () => void;
    const released = new Promise<void>((r) => (release = r));
    const answers: number[] = []; // how many entries each call was answered with, in the order the calls came
    await page.route(GET_HISTORY, async (route) => {
      const snapshot = entries; // what the server knew when this call arrived
      const call = answers.push(snapshot.length);
      if (call === 1) {
        firstArrived();
        await released; // the first answer is held back
      }
      await route.fulfill({ json: { entries: snapshot } }).catch(() => undefined); // a superseded request may already be gone
    });

    await page.goto(`/orders/${id}`);
    await firstIsHeld; // positive control: the first call is on its way, and it was answered from the CREATED-only state
    expect(answers).toEqual([1]);
    status = finalStatus;
    entries = full;
    await expect(page.getByTestId("order-status")).toHaveText(final, { timeout: 10_000 }); // the order is final before any history answered

    release(); // the stale first answer now reaches the page
    await expect(rows(page)).toHaveCount(2, { timeout: 10_000 });
    expect(await texts(page, "history-event")).toEqual(["CREATED", final]);
    // Positive control: the second call is what brought the row, and its answer really held it.
    expect(answers.length).toBeGreaterThanOrEqual(2);
    expect(answers[1]).toBe(2);

    // A final order is complete now: it is not asked again over more than two of the order's poll periods.
    const settled = answers.length;
    await page.waitForTimeout(4_500);
    expect(answers.length).toBe(settled);
  });
}
