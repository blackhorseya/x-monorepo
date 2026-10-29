import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { useKey } from "./helpers";

// Which parts of the console a caller without the admin role sees. The server stays the gate
// (every admin RPC checks the role); this is what the console shows and, for the admin pages,
// what it does not even ask. Runs in the auth project: auth.mode=required, ADMIN_KEY holds the
// admin role, OPS_KEY none.

const adminPages = ["Poison", "Prune", "Expire", "Accounts"] as const;
const navLink = (page: Page, name: string) => page.locator("header nav").getByRole("link", { name, exact: true });

test("no admin role: the nav leaves out every admin page", async ({ page }) => {
  await page.goto("/orders");
  await useKey(page, OPS_KEY);
  await expect(navLink(page, "Orders")).toBeVisible();
  await expect(navLink(page, "Stock")).toBeVisible();
  for (const name of adminPages) await expect(navLink(page, name)).toHaveCount(0);

  await useKey(page, ADMIN_KEY);
  for (const name of adminPages) await expect(navLink(page, name)).toBeVisible();
});

for (const path of ["/poison", "/prune", "/expire"]) {
  test(`no admin role: ${path} shows the gate and sends no admin RPC`, async ({ page }) => {
    const adminCalls: string[] = [];
    page.on("request", (req) => {
      if (req.method() === "POST" && /\/(admin\.v1\.AdminService|inventory\.v1\.InventoryService\/ExpireReservations)/.test(req.url())) {
        adminCalls.push(req.url());
      }
    });
    await page.goto("/orders");
    await useKey(page, OPS_KEY);
    await expect(navLink(page, "Orders")).toBeVisible();

    await page.goto(path);
    await expect(page.getByText("Admin role required")).toBeVisible();
    await page.waitForTimeout(500); // a page that mounted behind the gate would have asked by now
    expect(adminCalls).toEqual([]);

    await useKey(page, ADMIN_KEY);
    await expect(page.getByText("Admin role required")).toHaveCount(0);
  });
}

test("no admin role: Stock has no Set form, an admin does", async ({ page }) => {
  await page.goto("/stock");
  await useKey(page, OPS_KEY);
  await expect(page.locator('[data-slot="card-title"]', { hasText: "Stock" }).first()).toBeVisible();
  await expect(page.getByRole("button", { name: "Set", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Edit" })).toHaveCount(0);

  await useKey(page, ADMIN_KEY);
  await expect(page.getByRole("button", { name: "Set", exact: true })).toBeVisible();
});
