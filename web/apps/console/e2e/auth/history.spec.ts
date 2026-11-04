import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY } from "../../playwright.config";
import { seedProduct } from "../catalog";
import { createCard, createOrder, confirm } from "../orders-ui";
import { uid } from "../uid";
import { mintJWT } from "./jwt";
import { settle, useJWT } from "./helpers";

// The History card against a server that wants a credential (auth.mode=required, in memory): who may read an
// order's history, who is shown which actor_subject, and what a credential change takes away. The rows are
// written by real calls with real tokens, so the admin's view holds a non-empty subject (the control every
// "no subject" below is compared with). The server decides who sees a subject; the console must not keep
// showing an answer after the credential it was for is gone.

const HISTORY = "**/ordering.v1.OrderService/GetOrderHistory";
const rows = (page: Page) => page.getByTestId("history-row");

async function aliceOrder(page: Page, request: import("@playwright/test").APIRequestContext, alice: string): Promise<string> {
  const sku = `E2E-HA-${uid()}`;
  await seedProduct(request, sku, { headers: { "X-API-Key": ADMIN_KEY } });
  await page.goto("/orders");
  await useJWT(page, mintJWT(alice));
  await createCard(page).getByLabel("SKU").fill(sku);
  const id = await createOrder(page);
  await page.goto(`/orders/${id}`);
  await page.getByRole("button", { name: "Cancel", exact: true }).click(); // a second CUSTOMER row by alice
  await confirm(page, "Cancel order");
  await expect(page.getByTestId("order-status")).toHaveText("CANCELLED");

  return id;
}

test("only an admin is shown who acted; the owner sees the same rows without it", async ({ page, request }) => {
  const alice = `e2e-alice-${uid()}`;
  await aliceOrder(page, request, alice);

  // Control: the admin's view holds the subject, on both rows.
  await useJWT(page, mintJWT(`e2e-ops-${uid()}`, { roles: ["admin"] }));
  await expect(rows(page)).toHaveCount(2);
  expect(await page.getByTestId("history-subject").allTextContents()).toEqual([alice, alice]);

  // The owner: same rows, kinds shown, no subject anywhere in the card.
  await useJWT(page, mintJWT(alice));
  await expect(rows(page)).toHaveCount(2);
  expect(await page.getByTestId("history-actor").allTextContents()).toEqual(["Customer", "Customer"]);
  await settle(page);
  await expect(page.getByTestId("history-subject")).toHaveCount(0);
  await expect(page.getByTestId("history-card")).not.toContainText(alice);
});

test("another customer is told there is no such order, and sees no history", async ({ page, request }) => {
  const alice = `e2e-alice-${uid()}`;
  await aliceOrder(page, request, alice);

  await useJWT(page, mintJWT(alice));
  await expect(rows(page)).toHaveCount(2); // control: the owner has rows
  await useJWT(page, mintJWT(`e2e-bob-${uid()}`));
  await expect(page.getByTestId("history-not-found")).toBeVisible();
  await expect(rows(page)).toHaveCount(0);
  await expect(page.getByTestId("history-card")).not.toContainText(alice);
});

test("after the admin switches to a non-admin, the subject is gone", async ({ page, request }) => {
  const alice = `e2e-alice-${uid()}`;
  await aliceOrder(page, request, alice);

  await useJWT(page, mintJWT(`e2e-ops-${uid()}`, { roles: ["admin"] }));
  await expect(page.getByTestId("history-subject")).toHaveCount(2); // control: it was there

  await useJWT(page, mintJWT(`e2e-other-${uid()}`)); // a non-admin who is not the owner
  await settle(page);
  await expect(page.getByTestId("history-subject")).toHaveCount(0);
  await expect(page.getByTestId("history-card")).not.toContainText(alice);
});

test("after sign-out the history and its subject are gone", async ({ page, request }) => {
  const alice = `e2e-alice-${uid()}`;
  await aliceOrder(page, request, alice);

  await useJWT(page, mintJWT(`e2e-ops-${uid()}`, { roles: ["admin"] }));
  await expect(page.getByTestId("history-subject")).toHaveCount(2); // control

  await page.locator("header").getByRole("button", { name: "Clear" }).click();
  await expect(page.getByTestId("history-error")).toContainText("auth.unauthenticated");
  await expect(rows(page)).toHaveCount(0);
  await expect(page.getByTestId("history-subject")).toHaveCount(0);
  await expect(page.getByTestId("history-card")).not.toContainText(alice);
});

test("an admin's slow answer that arrives after the switch is not shown", async ({ page, request }) => {
  const alice = `e2e-alice-${uid()}`;
  const id = await aliceOrder(page, request, alice);
  const adminToken = mintJWT(`e2e-ops-${uid()}`, { roles: ["admin"] });

  let received!: () => void;
  const hasReceived = new Promise<void>((r) => (received = r));
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  let delivered!: () => void;
  const wasDelivered = new Promise<void>((r) => (delivered = r));
  let heldBody = "";
  let held = false;
  await page.route(HISTORY, async (route) => {
    if (!held && route.request().headers()["authorization"] === `Bearer ${adminToken}`) {
      held = true; // the first admin call is the one held; the real server answers it, the page gets it late
      const real = await route.fetch();
      heldBody = await real.text();
      received();
      await released;
      await route.fulfill({ response: real, body: heldBody });
      delivered();

      return;
    }
    await route.continue();
  });

  await page.goto(`/orders/${id}`);
  await useJWT(page, adminToken);
  await hasReceived; // positive control: the admin's call is on its way
  expect(heldBody).toContain(alice); // and the real answer to it names the actor

  const nonAdmin = mintJWT(alice);
  await useJWT(page, nonAdmin);
  await expect(rows(page)).toHaveCount(2); // the new credential's own answer is on screen
  await expect(page.getByTestId("history-subject")).toHaveCount(0);

  release();
  await wasDelivered; // positive control: the stale answer has been handed to the page
  await settle(page);
  await page.waitForTimeout(300);
  await expect(page.getByTestId("history-subject")).toHaveCount(0); // a snapshot: nothing later repairs a subject shown
  await expect(page.getByTestId("history-card")).not.toContainText(alice);
});
