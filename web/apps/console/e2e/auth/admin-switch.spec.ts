import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { settle, useKey } from "./helpers";
import { uid } from "../uid";

// Admin pages mount their queries behind AdminGate, but the credential control invalidates every
// active query the instant the credential changes, before the gate has closed. Whatever the new
// credential is, an admin-only procedure must not be sent with it: not a role-less key, and not
// anonymously after "Clear".

const adminOnly = (url: string) =>
  /\/api\/(admin\.v1\.AdminService\/|identity\.v1\.IdentityService\/(?!WhoAmI|ClaimInvite))/.test(url) || url.includes("UpsertProduct") || url.includes("DeactivateProduct");

type Switch = { name: string; apply: (page: Page) => Promise<void>; sends: (headers: Record<string, string>) => boolean };
const switches: Switch[] = [
  { name: "a role-less key", apply: (p) => useKey(p, OPS_KEY), sends: (h) => h["x-api-key"] === OPS_KEY },
  {
    name: "Clear (anonymous)",
    apply: async (p) => {
      await p.getByRole("button", { name: "Clear" }).click();
    },
    sends: (h) => !h["x-api-key"] && !h["authorization"],
  },
];

for (const sw of switches) {
  for (const path of ["/accounts", "ACCOUNT", "/products", "/poison", "/prune", "/expire"]) {
    test(`admin on ${path} switches to ${sw.name}: no admin-only RPC is sent with the new credential`, async ({ page }) => {
      await page.goto("/orders");
      await useKey(page, ADMIN_KEY);
      let target = path;
      if (path === "ACCOUNT") {
        const name = `e2e-switch-${uid()}`;
        await page.goto("/accounts");
        await page.getByLabel("Name", { exact: true }).fill(name);
        await page.getByRole("button", { name: "Create account" }).click();
        await page.getByRole("row").filter({ hasText: name }).getByRole("link", { name }).click();
        await expect(page.getByText("No sign-ins bound yet")).toBeVisible();
        target = "";
      } else {
        await page.goto(path);
        await expect(page.getByText("Admin role required")).toHaveCount(0);
        await settle(page);
      }
      void target;

      const leaked: string[] = [];
      page.on("request", (r) => {
        // The admin probe (ListAccounts, pageSize 1) is the one admin-only call that SHOULD go out with the new
        // credential: it is how the console finds out whether that credential is an admin.
        const probe = r.url().endsWith("IdentityService/ListAccounts") && /"pageSize":\s*1\b/.test(r.postData() ?? "");
        if (adminOnly(r.url()) && !probe && sw.sends(r.headers())) leaked.push(r.url().split("/api/")[1]);
      });
      await sw.apply(page);
      await expect(page.getByText("Admin role required")).toBeVisible();
      await settle(page);
      await page.waitForTimeout(500);
      expect(leaked).toEqual([]);
    });
  }
}
