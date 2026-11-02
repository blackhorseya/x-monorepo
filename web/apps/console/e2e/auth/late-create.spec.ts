import { expect, test } from "@playwright/test";
import { ADMIN_KEY } from "../../playwright.config";
import { seedProduct } from "../catalog";
import { uid } from "../uid";
import { settle, useJWT } from "./helpers";
import { mintJWT } from "./jwt";

// CreateOrder opens the new order's page on success. A success that arrives after the console has
// switched to another credential belongs to the old one: the order was placed for the old
// customer, and the new credential may not read it, so it must not move the page.

const ORDER_PAGE = /\/orders\/[^/?]+$/;

test("a success for the previous credential does not open its order for the new one", async ({ page }) => {
  const sku = `E2E-LATE-${uid()}`;
  await seedProduct(page.request, sku, { headers: { "X-API-Key": ADMIN_KEY } });
  const alice = `e2e-alice-${uid()}`;
  const bob = `e2e-bob-${uid()}`;

  await page.goto("/orders");
  await useJWT(page, mintJWT(alice));
  await page.getByLabel("SKU").fill(sku);

  // alice's CreateOrder reaches the server and commits; the answer is held back.
  let committed!: () => void;
  const hasCommitted = new Promise<void>((r) => (committed = r));
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  let orderId = "";
  await page.route("**/ordering.v1.OrderService/CreateOrder", async (route) => {
    const res = await route.fetch();
    orderId = ((await res.json()) as { order: { id: string } }).order.id;
    committed();
    await released;
    await route.fulfill({ response: res });
  });
  await page.getByRole("button", { name: "Create order" }).click();
  await hasCommitted;
  expect(orderId).not.toBe(""); // positive control: the order exists, the answer is what is late

  await useJWT(page, mintJWT(bob));
  release();
  await expect.poll(() => page.evaluate(() => performance.getEntriesByType("resource").filter((e) => e.name.endsWith("/CreateOrder")).length)).toBeGreaterThanOrEqual(1);
  await settle(page);
  await page.waitForTimeout(300); // the answer has been delivered; only the page's reaction is left to wait for

  // A snapshot, not a retrying expect: a later navigation would not repair a regression.
  expect(new URL(page.url()).pathname).toBe("/orders");
  expect(await page.getByText("No such order").count()).toBe(0);
});

// The other side: an answer for the credential still in use opens the order.
test("a success for the current credential opens the order", async ({ page }) => {
  const sku = `E2E-LATE-${uid()}`;
  await seedProduct(page.request, sku, { headers: { "X-API-Key": ADMIN_KEY } });
  await page.goto("/orders");
  await useJWT(page, mintJWT(`e2e-carol-${uid()}`));
  await page.getByLabel("SKU").fill(sku);
  await page.getByRole("button", { name: "Create order" }).click();
  await expect(page).toHaveURL(ORDER_PAGE);
});

// bob's own attempt is half done (the answer to his first press was lost, so the console keeps his
// key for the re-send). alice's late success arrives meanwhile: it ends alice's intent, not bob's.
test("a late success for the previous credential does not end the current credential's intent", async ({ page }) => {
  const sku = `E2E-LATE-${uid()}`;
  await seedProduct(page.request, sku, { headers: { "X-API-Key": ADMIN_KEY } });

  type Sent = { idempotencyKey?: string };
  const sent: Sent[] = [];
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  let aliceCommitted!: () => void;
  const aliceHasCommitted = new Promise<void>((r) => (aliceCommitted = r));
  let bobLost!: () => void;
  const bobHasLost = new Promise<void>((r) => (bobLost = r));
  await page.route("**/ordering.v1.OrderService/CreateOrder", async (route) => {
    sent.push(route.request().postDataJSON() as Sent);
    const call = sent.length; // 1 = alice (answer held), 2 = bob (answer lost), 3 = bob's re-send
    if (call === 1) {
      const res = await route.fetch();
      aliceCommitted();
      await released;
      await route.fulfill({ response: res });
    } else if (call === 2) {
      await route.fetch();
      await route.abort("connectionreset");
      bobLost();
    } else {
      await route.continue();
    }
  });

  await page.goto("/orders");
  await useJWT(page, mintJWT(`e2e-alice-${uid()}`));
  await page.getByLabel("SKU").fill(sku);
  await page.getByRole("button", { name: "Create order" }).click();
  await aliceHasCommitted;

  await useJWT(page, mintJWT(`e2e-bob-${uid()}`));
  await page.getByLabel("SKU").fill(sku);
  await page.getByRole("button", { name: "Create order" }).click();
  await bobHasLost;
  await expect(page.getByRole("button", { name: "Create order" })).toBeEnabled();

  release(); // alice's answer arrives now
  await settle(page);
  await page.waitForTimeout(300);

  await page.getByRole("button", { name: "Create order" }).click();
  await expect.poll(() => sent.length).toBe(3);
  expect(sent[1].idempotencyKey).toBeTruthy(); // positive control: bob's first press carried a key
  expect(sent[2].idempotencyKey).toBe(sent[1].idempotencyKey);
});
