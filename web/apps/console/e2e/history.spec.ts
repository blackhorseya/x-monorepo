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

// A catch-up that fails must not stop the card for good: the next status the order reaches is a new reason to ask
// (#442 r1), and the same status is not (a failure is shown once, not retried in a loop). The order sits in CONFIRMED
// (history polled slowly: 15s) while the catch-up for it fails; the history request that the interval fires at the
// same moment as the order's next poll is refused too, so only the request the new status itself triggers can fill the row.
//
// The matrix, each cell a real page with every wait on a request, a reply or what the page shows:
//   A  first reply in flight -> final            the two tests above
//   B  catch-up fails -> non-final -> final      "B" below (PAID in between)
//   C  catch-up fails -> final                   "C" below, COMPLETED and CANCELLED
//   D  catch-up fails -> status unchanged        "D": no new request over a window longer than the order's 2s period
//   E  catch-up fails -> next catch-up succeeds  asserted in B and C: the error is shown, then gone with the full rows
function failedCatchUp(page: Page, id: string) {
  const created = entry(1, "ORDER_STATUS_UNSPECIFIED", "ORDER_STATUS_PENDING");
  const confirmed = entry(2, "ORDER_STATUS_PENDING", "ORDER_STATUS_CONFIRMED");
  const s = {
    created,
    confirmed,
    status: "ORDER_STATUS_PENDING",
    entries: [created],
    finalStatus: "",
    phase: "ok" as "ok" | "refuse" | "served", // GetOrderHistory: answers / refuses (503) / answers once the final order has been served
    answers: [] as string[],
  };
  const install = async () => {
    await page.route(NO_RESERVATION, (route) => route.fulfill({ status: 404, json: { code: "not_found", message: "none" } }));
    await page.route(GET_ORDER, async (route) => {
      if (s.finalStatus !== "" && s.status === s.finalStatus) {
        // Latency injected on the final order's answer only: a history request fired in the same timer tick as this
        // poll reaches the server while the phase is still "refuse"; whatever the new status triggers comes after.
        await new Promise((r) => setTimeout(r, 400));
        s.phase = "served";
      }
      await route.fulfill({ json: { order: order(id, s.status) } });
    });
    await page.route(GET_HISTORY, (route) => {
      if (s.phase === "refuse") {
        // A refusal the card does not retry by itself (queries.ts historyQuery): a 503 would be retried a second and a
        // third time, and a retry that lands after the phase has changed would answer and hide what is being tested.
        s.answers.push("401");
        return route.fulfill({ status: 401, headers: { "x-error-code": "auth.unauthenticated" }, json: { code: "unauthenticated", message: "refused" } });
      }
      s.answers.push(`ok:${s.entries.length}`);
      return route.fulfill({ json: { entries: s.entries } });
    });
  };
  // Up to the failed catch-up for CONFIRMED: the card shows its first snapshot (1 row) and the error.
  const untilFailed = async () => {
    await install();
    await page.goto(`/orders/${id}`);
    await expect(rows(page)).toHaveCount(1);
    s.status = "ORDER_STATUS_CONFIRMED";
    s.entries = [created, confirmed];
    s.phase = "refuse";
    await expect(page.getByTestId("order-status")).toHaveText("CONFIRMED", { timeout: 10_000 });
    await expect(page.getByTestId("history-error")).toBeVisible({ timeout: 15_000 }); // the catch-up for CONFIRMED failed
    await expect(rows(page)).toHaveCount(1); // and the card still shows the snapshot it had
  };

  return { s, untilFailed };
}

for (const final of ["COMPLETED", "CANCELLED"] as const) {
  test(`C: a failed catch-up does not stop the card from catching up when the order turns ${final}`, async ({ page }) => {
    const { s, untilFailed } = failedCatchUp(page, `e2e-hist-c-${final}-${uid()}`);
    await untilFailed();

    s.finalStatus = `ORDER_STATUS_${final}`;
    s.status = s.finalStatus;
    s.entries = [s.created, s.confirmed, entry(3, "ORDER_STATUS_CONFIRMED", s.finalStatus, { cause: "ORDER_HISTORY_CAUSE_RPC_REQUEST" })];
    await expect(page.getByTestId("order-status")).toHaveText(final, { timeout: 25_000 });
    await expect(rows(page)).toHaveCount(3, { timeout: 15_000 }); // the new status asked again, and the answer held the last row
    expect(await texts(page, "history-event")).toEqual(["CREATED", "CONFIRMED", final]);
    expect(s.answers).toContain("ok:3"); // positive control: the request that brought the row really was answered with it
    await expect(page.getByTestId("history-error")).toHaveCount(0); // E: the failure is gone with the full rows
  });
}

test("B: a failed catch-up, then a non-final status, then a final one: the last asks again and the rows are complete", async ({ page }) => {
  const { s, untilFailed } = failedCatchUp(page, `e2e-hist-b-${uid()}`);
  await untilFailed();

  // The order moves on to PAID (polled at the fast pace): the card asks again for the new status. Its answer is
  // refused too (the phase has not changed), which is what lets the next step prove that failure never sticks.
  const before = s.answers.length;
  s.status = "ORDER_STATUS_PAID";
  s.entries = [s.created, s.confirmed, entry(3, "ORDER_STATUS_CONFIRMED", "ORDER_STATUS_PAID", { cause: "ORDER_HISTORY_CAUSE_PAYMENT_CAPTURED" })];
  await expect(page.getByTestId("order-status")).toHaveText("PAID", { timeout: 25_000 });
  await expect.poll(() => s.answers.length, { timeout: 10_000 }).toBeGreaterThan(before); // a new request for the new status
  await expect(rows(page)).toHaveCount(1); // still refused: the old snapshot

  s.finalStatus = "ORDER_STATUS_COMPLETED";
  s.status = s.finalStatus;
  s.entries = [...s.entries, entry(4, "ORDER_STATUS_PAID", "ORDER_STATUS_COMPLETED", { cause: "ORDER_HISTORY_CAUSE_RPC_REQUEST" })];
  await expect(page.getByTestId("order-status")).toHaveText("COMPLETED", { timeout: 15_000 });
  await expect(rows(page)).toHaveCount(4, { timeout: 15_000 });
  expect(await texts(page, "history-event")).toEqual(["CREATED", "CONFIRMED", "PAID", "COMPLETED"]);
  expect(s.answers).toContain("ok:4");
  await expect(page.getByTestId("history-error")).toHaveCount(0);
});

test("D: a failed catch-up is not retried while the status stays the same", async ({ page }) => {
  const { s, untilFailed } = failedCatchUp(page, `e2e-hist-d-${uid()}`);
  await untilFailed();

  // The attempt (with its own retries) is over: nothing asks again over more than two periods of the order's 2s poll.
  const settled = s.answers.length;
  await page.waitForTimeout(4_500);
  expect(s.answers.length).toBe(settled);
  await expect(page.getByTestId("history-error")).toBeVisible(); // and the failure is still what the card shows
});
