import { expect, test } from "@playwright/test";
import { ADMIN_KEY } from "../../playwright.config";
import { subject, useJWT, useKey } from "./helpers";
import { mintJWT } from "./jwt";
import { uid } from "../uid";

// The parts of the credential story auth.spec.ts leaves out: the JWT kind
// (header shows the token's sub; expired / wrong issuer / wrong secret are
// auth.invalid_credential; the roles claim is what the admin gate reads), and
// that mutations carry the credential too — the interceptor is read at call
// time, so a key set after the page loaded must reach the next Create.

test("JWT: sub in the header, rejected variants name the reason", async ({
  page,
}) => {
  await page.goto("/accounts");

  // ListAccounts is an admin RPC: the roles claim is what lets alice see the list.
  await useJWT(page, mintJWT("alice", { roles: ["admin"] }));
  await expect(subject(page)).toHaveText("jwt · alice");
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(
    page
      .getByRole("row")
      .filter({ hasText: /No accounts yet|ACTIVE|DISABLED/ })
      .first()
  ).toBeVisible();

  // Survives a reload like the API key does.
  await page.reload();
  await expect(subject(page)).toHaveText("jwt · alice");
  await expect(page.getByRole("alert")).toHaveCount(0);

  for (const bad of [
    mintJWT("alice", { roles: ["admin"], exp: Math.floor(Date.now() / 1000) - 60 }), // expired
    mintJWT("alice", { roles: ["admin"], iss: "https://someone-else.example.com" }), // wrong issuer
    mintJWT("alice", { roles: ["admin"] }, "another-secret-0123456789abcdef0123456789abcdef"), // wrong key
  ]) {
    await useJWT(page, bad);
    const alert = page.getByRole("alert").first();
    await expect(alert).toContainText("Not authenticated");
    await expect(alert).toContainText("auth.invalid_credential");
  }
  // The console still labels it by its payload: the server, not the client, decided.
  await expect(subject(page)).toHaveText("jwt · alice");
});

test("JWT roles claim drives the admin gate", async ({ page }) => {
  await page.goto("/prune");
  await useJWT(page, mintJWT("bob"));
  await expect(page.getByText("Admin role required")).toBeVisible();
  await expect(page.getByRole("button", { name: "Dry run", exact: true })).toHaveCount(0);

  await useJWT(page, mintJWT("carol", { roles: ["admin"] }));
  await page.getByRole("button", { name: "Dry run", exact: true }).click();
  // Past the gate: this server has no database, so the next error is admin's own.
  await expect(page.getByRole("alert")).toContainText("admin.no_database");
});

test("mutations carry the credential set after the page loaded", async ({ page }) => {
  const name = `e2e-auth-write-${uid()}`;
  await page.goto("/accounts");
  await expect(page.getByRole("alert").first()).toContainText("auth.unauthenticated");

  // Anonymous Create is refused by the server, and says so next to the form.
  await page.getByLabel("Name").fill(name);
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.getByRole("alert").filter({ hasText: "auth.unauthenticated" })).toHaveCount(2);

  // An admin key: the list recovers at once and the form's stale error goes with it
  // (the credential change resets mutations); the same Create now goes through.
  await useKey(page, ADMIN_KEY);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(page.getByRole("row").filter({ hasText: name })).toHaveCount(1);

  // Clear (the header's): the next write is anonymous again.
  await page.locator("header").getByRole("button", { name: "Clear" }).click();
  await page.getByLabel("Name").fill(`${name}-2`);
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.getByRole("alert").filter({ hasText: "auth.unauthenticated" }).first()).toBeVisible();
});
