import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { useKey } from "./helpers";
import { uid } from "../uid";

// /accounts: DB-managed accounts and their API keys (identity.v1.IdentityService).
// Runs in the auth project: the server is auth.mode=required, so the admin gate is
// real. ADMIN_KEY holds the admin role, OPS_KEY none. The secret CreateApiKey
// returns is shown once, in the dialog, and must be nowhere afterwards.

const nav = (page: Page) => page.locator("header nav").getByRole("link", { name: "Accounts" });

test("no admin role: no nav entry, and the route says so", async ({ page }) => {
  await page.goto("/orders");
  await useKey(page, OPS_KEY);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(page.locator("header nav").getByRole("link", { name: "Orders" })).toBeVisible();
  await expect(nav(page)).toHaveCount(0);

  await page.goto("/accounts");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await expect(page.getByRole("button", { name: "Create account" })).toHaveCount(0);

  await useKey(page, ADMIN_KEY);
  await expect(nav(page)).toBeVisible();
  await expect(page.getByText("Admin role required")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Create account" })).toBeVisible();
});

test("admin: create account → key (secret shown once) → revoke → disable / enable", async ({ page }) => {
  const name = `e2e-acct-${uid()}`;
  await page.goto("/accounts");
  await useKey(page, ADMIN_KEY);

  // Create the account.
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByLabel("Roles").fill("viewer, ops");
  await page.getByRole("button", { name: "Create account" }).click();
  const row = page.getByRole("row").filter({ hasText: name });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText("ACTIVE");
  await expect(row).toContainText("ops");
  await expect(row).toContainText("viewer");

  // Detail page.
  await row.getByRole("link", { name }).click();
  await expect(page).toHaveURL(/\/accounts\/.+/);
  const accountURL = page.url();
  await expect(page.locator('[data-slot="card-title"]', { hasText: "API keys" })).toBeVisible();
  await expect(page.getByText("No API keys yet.")).toBeVisible();

  // Create a key: the secret is in the dialog, copyable, and only there.
  await page.getByRole("button", { name: "New API key" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("Key name").fill("ci");
  await dialog.getByRole("button", { name: "Create key" }).click();
  const secretBox = dialog.getByTestId("api-key-secret");
  await expect(secretBox).toBeVisible();
  const secret = (await secretBox.textContent())?.trim() ?? "";
  expect(secret).toMatch(/^xk_[A-Za-z0-9_-]{43}$/);
  await expect(dialog.getByRole("button", { name: "Copy" })).toBeVisible();
  // While open it is still not in the URL or in any storage.
  expect(page.url()).not.toContain(secret);
  expect(await storageDump(page)).not.toContain(secret);

  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);

  // Gone: page text, URL, storage — and it survives neither a re-render nor a reload.
  await expect(page.locator("body")).not.toContainText(secret);
  expect(page.url()).toBe(accountURL);
  expect(await storageDump(page)).not.toContain(secret);
  await page.reload();
  await expect(page.locator("body")).not.toContainText(secret);
  expect(await storageDump(page)).not.toContain(secret);

  // The list shows the prefix (first 8 chars), never the secret.
  const keyRow = page.getByRole("row").filter({ hasText: "ci" });
  await expect(keyRow).toHaveCount(1);
  await expect(keyRow).toContainText(secret.slice(0, 8));
  await expect(keyRow).toContainText("ACTIVE");
  await expect(keyRow).toContainText("never"); // expires_at

  // Revoke asks first; cancel leaves it active.
  await keyRow.getByRole("button", { name: "Revoke" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel" }).click();
  await expect(keyRow).toContainText("ACTIVE");
  await keyRow.getByRole("button", { name: "Revoke" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Revoke key" }).click();
  await expect(keyRow).toContainText("REVOKED");
  await expect(keyRow.getByRole("button", { name: "Revoke" })).toHaveCount(0);

  // Roles: replace the whole set.
  await page.getByLabel("Roles").fill("auditor");
  await page.getByRole("button", { name: "Save roles" }).click();
  await expect(page.getByText("auditor")).toBeVisible();

  // Disable asks first; enable does not.
  await page.getByRole("link", { name: "Accounts", exact: true }).first().click();
  await expect(page).toHaveURL(/\/accounts$/);
  await row.getByRole("button", { name: "Disable" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel" }).click();
  await expect(row).toContainText("ACTIVE");
  await row.getByRole("button", { name: "Disable" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Disable account" }).click();
  await expect(row).toContainText("DISABLED");
  await row.getByRole("button", { name: "Enable" }).click();
  await expect(row).toContainText("ACTIVE");
});

// Revoke taking effect is the resolver cache's 30s contract (adapters/auth/resolver_test.go), too slow for here.
test("the key the dialog showed really authenticates", async ({ page, request }) => {
  const name = `e2e-key-${uid()}`;
  await page.goto("/accounts");
  await useKey(page, ADMIN_KEY);
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByRole("button", { name: "Create account" }).click();
  await page.getByRole("row").filter({ hasText: name }).getByRole("link", { name }).click();
  await page.getByRole("button", { name: "New API key" }).click();
  await page.getByRole("dialog").getByLabel("Key name").fill("probe");
  await page.getByRole("dialog").getByRole("button", { name: "Create key" }).click();
  const secret = (await page.getByTestId("api-key-secret").textContent())?.trim() ?? "";
  await page.getByRole("dialog").getByRole("button", { name: "Done" }).click();

  const whoami = () =>
    request.post(`${new URL(page.url()).origin}/api/identity.v1.IdentityService/WhoAmI`, {
      headers: { "Content-Type": "application/json", "X-API-Key": secret },
      data: {},
    });
  expect((await whoami()).status()).toBe(200);
});

// Everything the page could have persisted, as one string.
async function storageDump(page: Page): Promise<string> {
  return page.evaluate(() =>
    JSON.stringify([
      Object.entries(localStorage),
      Object.entries(sessionStorage),
      document.cookie,
    ]),
  );
}

// Switching credential while the previous one's admin check is still in flight: the late answer
// belongs to the old credential and must not open the admin UI for the new one.
test("a late admin answer does not outlive a switch to a role-less key", async ({ page }) => {
  await page.goto("/orders");

  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  let held!: () => void;
  const heldAdmin = new Promise<void>((r) => (held = r));
  const seen: string[] = [];
  await page.route("**/identity.v1.IdentityService/*", async (route) => {
    const key = route.request().headers()["x-api-key"];
    seen.push(key === ADMIN_KEY ? "admin" : key === OPS_KEY ? "ops" : "other");
    if (key !== ADMIN_KEY) return route.continue();
    const res = await route.fetch(); // the server has already answered: admin
    held();
    await released; // ... but the browser hears it late
    await route.fulfill({ response: res });
  });

  await useKey(page, ADMIN_KEY);
  await heldAdmin;
  await useKey(page, OPS_KEY);
  await expect.poll(() => seen.includes("ops")).toBe(true); // the new credential was checked on its own
  release();

  await page.waitForTimeout(1500); // let the stale answer land and render
  await expect(nav(page)).toHaveCount(0);
  await page.goto("/accounts");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await expect(page.getByRole("button", { name: "Create account" })).toHaveCount(0);
});
