import { expect, test, type Page } from "@playwright/test";
import { seedProduct } from "./catalog";
import { uid } from "./uid";

// What /orders/$id is allowed to poll, and when it must stop. Mode-agnostic: with a worker (CI)
// the real order and reservation move on their own, so the order and reservation answers are
// scripted over a real order; the console's request pattern is what is measured. A fake clock
// stands in for the 15s poll (page.clock), and every window starts with a positive control (the
// request was made at all), so "no more requests" cannot pass because nothing was ever asked.

const ORDER = "ordering.v1.OrderService/GetOrder";
const RESERVATION = "inventory.v1.InventoryService/GetReservation";
const SLOW = 15_000; // the slow poll: an order that can still change by itself, a reservation still RESERVED

async function realOrder(page: Page) {
  const sku = `E2E-OP-${uid()}`;
  await seedProduct(page.request, sku);
  const res = await page.request.post("/api/ordering.v1.OrderService/CreateOrder", {
    data: { customerId: `e2e-op-${uid()}`, items: [{ sku, quantity: 1 }] },
  });
  return ((await res.json()) as { order: { id: string } }).order.id;
}

// GetOrder answered with the real order and the status `state.order` says; GetReservation with
// `state.reservation` (null = NotFound). Both are counted.
function script(page: Page, state: { order: string; reservation: string | null }) {
  const calls = { order: 0, reservation: 0 };
  return page
    .route(`**/${ORDER}`, async (route) => {
      calls.order++;
      const body = (await (await route.fetch()).json()) as { order: { status: string } };
      body.order.status = `ORDER_STATUS_${state.order}`;
      await route.fulfill({ json: body });
    })
    .then(() =>
      page.route(`**/${RESERVATION}`, async (route) => {
        calls.reservation++;
        if (state.reservation === null) {
          return route.fulfill({ status: 404, json: { code: "not_found", message: "no reservation" } });
        }
        const { orderId } = route.request().postDataJSON() as { orderId: string };
        await route.fulfill({ json: { reservation: { orderId, status: `RESERVATION_STATUS_${state.reservation}`, lines: [], shortfallSkus: [] } } });
      }),
    )
    .then(() => calls);
}

// Let `ms` of fake time pass in steps, with real time in between for the requests it triggers.
async function elapse(page: Page, ms: number) {
  await page.clock.fastForward(ms);
  await page.waitForTimeout(400);
}

test("a CONFIRMED order is looked at again: the reservation expired, so the order is CANCELLED and Complete is gone", async ({ page }) => {
  await page.clock.install();
  const id = await realOrder(page);
  const state = { order: "CONFIRMED", reservation: "RESERVED" as string | null };
  const calls = await script(page, state);

  await page.goto(`/orders/${id}`);
  await expect(page.getByTestId("order-status")).toHaveText("CONFIRMED");
  await expect(page.getByRole("button", { name: "Complete" })).toBeVisible();
  expect(calls.order).toBeGreaterThanOrEqual(1); // positive control

  // Inventory expires the reservation and cancels the order behind the page's back.
  state.order = "CANCELLED";
  state.reservation = "EXPIRED";
  const before = calls.order;
  await elapse(page, 5_000);
  expect(calls.order).toBe(before); // not the 2s poll of a PENDING order
  await elapse(page, SLOW);
  await expect(page.getByTestId("order-status")).toHaveText("CANCELLED");
  await expect(page.getByRole("button", { name: "Complete" })).toHaveCount(0);
});

test("a COMPLETED order is not asked about again", async ({ page }) => {
  await page.clock.install();
  const id = await realOrder(page);
  const calls = await script(page, { order: "COMPLETED", reservation: "COMMITTED" });

  await page.goto(`/orders/${id}`);
  await expect(page.getByTestId("order-status")).toHaveText("COMPLETED");
  await expect(page.getByTestId("reservation-status")).toHaveText("COMMITTED");
  expect(calls.order).toBeGreaterThanOrEqual(1); // positive control
  expect(calls.reservation).toBeGreaterThanOrEqual(1);
  await page.waitForTimeout(400);
  const settled = { ...calls };

  for (let i = 0; i < 3; i++) await elapse(page, 20_000);
  expect(calls).toEqual(settled);
});

test("no such order: asked once, then the page stops", async ({ page }) => {
  await page.clock.install();
  let asked = 0;
  await page.route(`**/${ORDER}`, (route) => {
    asked++;
    return route.fulfill({ status: 404, json: { code: "not_found", message: "no such order" } });
  });

  await page.goto(`/orders/e2e-missing-${uid()}`);
  await expect(page.getByText("No such order")).toBeVisible();
  expect(asked).toBeGreaterThanOrEqual(1); // positive control
  await page.waitForTimeout(400);
  const settled = asked;

  for (let i = 0; i < 3; i++) await elapse(page, 20_000);
  expect(asked).toBe(settled);
});

test("a terminal order with no reservation stops asking for one", async ({ page }) => {
  await page.clock.install();
  const id = await realOrder(page);
  const calls = await script(page, { order: "CANCELLED", reservation: null });

  await page.goto(`/orders/${id}`);
  await expect(page.getByTestId("order-status")).toHaveText("CANCELLED");
  await expect(page.getByText("none yet")).toBeVisible();
  expect(calls.order).toBeGreaterThanOrEqual(1); // positive controls
  expect(calls.reservation).toBeGreaterThanOrEqual(1);
  await page.waitForTimeout(400);
  const settled = { ...calls };

  for (let i = 0; i < 3; i++) await elapse(page, 20_000);
  expect(calls).toEqual(settled);
});

// The other side of the same rule: a reservation that is not there *yet* is waited for while the
// order can still get one.
test("a PENDING order keeps looking for a reservation that has not appeared", async ({ page }) => {
  await page.clock.install();
  const id = await realOrder(page);
  const state = { order: "PENDING", reservation: null as string | null };
  const calls = await script(page, state);

  await page.goto(`/orders/${id}`);
  await expect(page.getByText("none yet")).toBeVisible();
  expect(calls.reservation).toBeGreaterThanOrEqual(1);

  state.reservation = "RESERVED"; // the worker got there
  await elapse(page, 2_100);
  await expect(page.getByTestId("reservation-status")).toHaveText("RESERVED");
});

test("a RESERVED reservation is polled every 15s, not every 2s, until it settles", async ({ page }) => {
  await page.clock.install();
  const id = await realOrder(page);
  const state = { order: "CONFIRMED", reservation: "RESERVED" as string | null };
  const calls = await script(page, state);

  await page.goto(`/orders/${id}`);
  await expect(page.getByTestId("reservation-status")).toHaveText("RESERVED");
  expect(calls.reservation).toBeGreaterThanOrEqual(1); // positive control
  await page.waitForTimeout(400);
  const before = calls.reservation;

  await elapse(page, 5_000);
  expect(calls.reservation).toBe(before);

  state.reservation = "RELEASED";
  await elapse(page, SLOW);
  await expect(page.getByTestId("reservation-status")).toHaveText("RELEASED");
  const settled = calls.reservation;
  for (let i = 0; i < 2; i++) await elapse(page, 20_000);
  expect(calls.reservation).toBe(settled); // RELEASED is final
});
