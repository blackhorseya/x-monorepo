import { expect, test } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";

import { subject, useKey } from "./helpers";

// Against the auth project's server (auth.mode=required, two API keys, no DB):
// what the console shows when the server wants a credential, how the header
// control sets one, that it survives a reload (localStorage) and that Clear
// goes back to anonymous. The admin role gate is covered on /poison. The JWT
// kind and the write paths are in flows.spec.ts.
//
// Past the role gate this server has no database, so what an admin sees on /poison is the server's own
// answer, admin.no_database: reaching it is what "the credential was accepted" looks like here.

test("anonymous → wrong key → right key → reload → clear", async ({ page }) => {
  await page.goto("/poison");
  await expect(subject(page)).toHaveText("anonymous");
  // ListPoison doubles as the admin probe: an answer of Unauthenticated is "not an admin", and the page shows the gate.
  await expect(page.getByText("Admin role required")).toBeVisible();

  await useKey(page, "not-a-key-0123456789");
  await expect(subject(page)).toHaveText("api key"); // the console keeps what you typed; the server decides
  await expect(page.getByText("Admin role required")).toBeVisible();

  await useKey(page, ADMIN_KEY);
  await expect(page.getByText("Admin role required")).toHaveCount(0);
  await expect(page.getByRole("alert")).toContainText("admin.no_database");

  await page.reload();
  await expect(subject(page)).toHaveText("api key");
  await expect(page.getByText("Admin role required")).toHaveCount(0);
  await expect(page.getByRole("alert")).toContainText("admin.no_database");

  await page.getByRole("button", { name: "Clear" }).click();
  await expect(subject(page)).toHaveText("anonymous");
  await expect(page.getByText("Admin role required")).toBeVisible();
});

test("/poison needs the admin role", async ({ page }) => {
  await page.goto("/poison");
  await useKey(page, OPS_KEY);
  await expect(page.getByText("Admin role required")).toBeVisible();

  await useKey(page, ADMIN_KEY);
  // Past the role gate: this server has no database, so the next error is admin's own.
  await expect(page.getByRole("alert")).toContainText("admin.no_database");
});
