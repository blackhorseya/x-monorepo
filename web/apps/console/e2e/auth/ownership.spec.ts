import { expect, test } from "@playwright/test";
import { useJWT } from "./helpers";
import { mintJWT } from "./jwt";
import { ADMIN_KEY } from "../../playwright.config";
import { seedProduct } from "../catalog";
import { uid } from "../uid";

// Ordering ownership through the console: a role-less caller is the customer of
// what they place and sees nothing else; an admin can order on behalf of one.

test("a non-admin sees only their own orders; an admin orders on behalf of a customer", async ({
  page,
  request,
}) => {
  const sku = `E2E-OWN-${uid()}`;
  await seedProduct(request, sku, { headers: { "X-API-Key": ADMIN_KEY } });
  const alice = `e2e-alice-${uid()}`;
  const bob = `e2e-bob-${uid()}`;
  const walkIn = `e2e-walkin-${uid()}`;
  const createdFor = (who: string) =>
    page.getByRole("row").filter({ hasText: who });

  // alice orders for herself: the Customer field is locked, her subject is used.
  await page.goto("/orders");
  await useJWT(page, mintJWT(alice));
  await expect(page.getByLabel("Customer").first()).toBeDisabled();
  await page.getByLabel("SKU").fill(sku);
  await page.getByRole("button", { name: "Create order" }).click();
  await expect(createdFor(alice)).toHaveCount(1);
  await expect(page.getByRole("alert")).toHaveCount(0);

  // bob sees none of it, and asking for alice's orders by name is refused.
  await useJWT(page, mintJWT(bob));
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(createdFor(alice)).toHaveCount(0);
  await expect(page.getByText("No orders")).toBeVisible();
  await page.getByLabel("Customer").nth(1).fill(alice);
  await page.getByRole("button", { name: "Filter" }).click();
  await expect(page.getByRole("alert").first()).toContainText(
    "order.permission_denied"
  );

  // An admin names a customer; that customer then finds the order in their own list.
  await useJWT(page, mintJWT(`e2e-ops-${uid()}`, { roles: ["admin"] }));
  await expect(page.getByLabel("Customer").first()).toBeEnabled();
  await page.getByLabel("Customer").first().fill(walkIn);
  await page.getByLabel("SKU").fill(sku);
  await page.getByRole("button", { name: "Create order" }).click();
  await page.getByLabel("Customer").nth(1).fill(walkIn);
  await page.getByRole("button", { name: "Filter" }).click();
  await expect(createdFor(walkIn)).toHaveCount(1);

  await useJWT(page, mintJWT(walkIn));
  await page.goto("/orders");
  await expect(createdFor(walkIn)).toHaveCount(1);
  await expect(createdFor(alice)).toHaveCount(0);
});
