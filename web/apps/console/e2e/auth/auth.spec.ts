import { expect, test } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";

import { subject, useKey } from "./helpers";

// Against the auth project's server (auth.mode=required, two API keys, no DB):
// what the console shows when the server wants a credential, how the header
// control sets one, that it survives a reload (localStorage) and that Clear
// goes back to anonymous. The admin role gate is covered on /poison. The JWT
// kind and the write paths are in flows.spec.ts.

test("anonymous → wrong key → right key → reload → clear", async ({ page }) => {
  await page.goto("/orders");
  await expect(subject(page)).toHaveText("anonymous");
  const alert = page.getByRole("alert").first();
  await expect(alert).toContainText("Not authenticated");
  await expect(alert).toContainText("auth.unauthenticated");
  await expect(alert).toContainText("set an API key or JWT");

  await useKey(page, "not-a-key-0123456789");
  await expect(page.getByRole("alert").first()).toContainText(
    "auth.invalid_credential"
  );
  await expect(subject(page)).toHaveText("api key"); // the console keeps what you typed; the server decides

  await useKey(page, OPS_KEY);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(
    page
      .getByRole("row")
      .filter({ hasText: /No orders|PENDING|CANCELLED/ })
      .first()
  ).toBeVisible();

  await page.reload();
  await expect(subject(page)).toHaveText("api key");
  await expect(page.getByRole("alert")).toHaveCount(0);

  await page.getByRole("button", { name: "Clear" }).click();
  await expect(subject(page)).toHaveText("anonymous");
  await expect(page.getByRole("alert").first()).toContainText(
    "auth.unauthenticated"
  );
});

test("/poison needs the admin role", async ({ page }) => {
  await page.goto("/poison");
  await useKey(page, OPS_KEY);
  await expect(page.getByRole("alert")).toContainText("auth.permission_denied");

  await useKey(page, ADMIN_KEY);
  // Past the role gate: this server has no database, so the next error is admin's own.
  await expect(page.getByRole("alert")).toContainText("admin.no_database");
});
