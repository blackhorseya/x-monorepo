import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { isProbe } from "../probe";
import { settle, useKey } from "./helpers";

// Admin pages mount their queries behind AdminGate, but the credential control invalidates every
// active query the instant the credential changes, before the gate has closed. Whatever the new
// credential is, an admin-only procedure must not be sent with it: not a role-less key, and not
// anonymously after "Clear".

const adminOnly = (url: string) => /\/api\/admin\.v1\.AdminService\//.test(url);

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
  for (const path of ["/poison", "/prune"]) {
    test(`admin on ${path} switches to ${sw.name}: no admin-only RPC is sent with the new credential`, async ({ page }) => {
      await page.goto("/prune");
      await useKey(page, ADMIN_KEY);
      await page.goto(path);
      await expect(page.getByText("Admin role required")).toHaveCount(0);
      await settle(page);

      const leaked: string[] = [];
      page.on("request", (r) => {
        // The admin probe (ListPoison, limit 1) is the one admin-only call that SHOULD go out with the new
        // credential: it is how the console finds out whether that credential is an admin.
        if (adminOnly(r.url()) && !isProbe(r) && sw.sends(r.headers())) leaked.push(r.url().split("/api/")[1]);
      });
      await sw.apply(page);
      await expect(page.getByText("Admin role required")).toBeVisible();
      await settle(page);
      await page.waitForTimeout(500);
      expect(leaked).toEqual([]);
    });
  }
}
