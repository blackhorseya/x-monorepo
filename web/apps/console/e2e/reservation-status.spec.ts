import { expect, test, type Page } from "@playwright/test";
import { seedProduct } from "./catalog";
import { expectReservationStatus } from "./reservation-ui";
import { uid } from "./uid";

// The order page's Reservation card against the real server, with GetReservation answered by the
// test: the states a worker would produce are stated, not waited for, so "none yet" and "RESERVED"
// can be held still and the status oracle shown to refuse what is not on the page.

const NO_RESERVATION = { status: 404, contentType: "application/json", body: JSON.stringify({ code: "not_found", message: "no reservation" }) };
const reservation = (orderId: string, status: string) => ({
  status: 200,
  contentType: "application/json",
  body: JSON.stringify({ reservation: { orderId, status, lines: [], shortfallSkus: [] } }),
});

async function openOrder(page: Page, request: import("@playwright/test").APIRequestContext, answer: (orderId: string) => object) {
  const sku = `E2E-RS-${uid()}`;
  await seedProduct(request, sku);
  const res = await request.post("/api/ordering.v1.OrderService/CreateOrder", { data: { customerId: `e2e-rs-${uid()}`, items: [{ sku, quantity: 1 }] } });
  expect(res.ok(), `CreateOrder: ${res.status()} ${await res.text()}`).toBe(true);
  const orderId = (await res.json()).order.id as string;
  await page.route("**/inventory.v1.InventoryService/GetReservation", (route) => route.fulfill(answer(orderId)));
  await page.goto(`/orders/${orderId}`);
  return page.locator('[data-slot="card"]', { hasText: "Reservation" }).last();
}

const SHORT = { timeout: 1_000 };

test("no reservation yet: no status passes, however the card's description reads", async ({ page, request }) => {
  const card = await openOrder(page, request, () => NO_RESERVATION);
  await expect(card.getByText("none yet")).toBeVisible();
  for (const status of ["RESERVED", "RELEASED", "COMMITTED"]) {
    await expect(expectReservationStatus(card, status, SHORT), `${status} on a page with no reservation`).rejects.toThrow();
  }
});

test("a RESERVED reservation is RESERVED and not RELEASED", async ({ page, request }) => {
  const card = await openOrder(page, request, (id) => reservation(id, "RESERVATION_STATUS_RESERVED"));
  await expectReservationStatus(card, "RESERVED");
  await expect(expectReservationStatus(card, "RELEASED", SHORT)).rejects.toThrow();
});

test("a RELEASED badge next to the description that says 'released' is one match, not a strict-mode failure", async ({ page, request }) => {
  const card = await openOrder(page, request, (id) => reservation(id, "RESERVATION_STATUS_RELEASED"));
  // What a text match sees once the badge is up: the description and the badge both.
  await expect(card.getByText("RELEASED")).toHaveCount(2);
  await expectReservationStatus(card, "RELEASED");
});
