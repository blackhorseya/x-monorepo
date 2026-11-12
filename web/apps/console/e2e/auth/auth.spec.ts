import { expect, test } from "@playwright/test";
import { ADMIN_KEY } from "../../playwright.config";

import { subject, useJWT, useKey } from "./helpers";
import { mintJWT } from "./jwt";

// Against the auth project's server (auth.mode=required, two API keys, no DB):
// what the console shows when the server wants a credential, how the header
// control sets one, that it survives a reload (localStorage) and that Clear
// goes back to anonymous. The admin role gate is covered on /poison. The JWT
// kind is the second test; the third is the write path: a mutation carries the credential set after the page loaded.
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

test("JWT: sub in the header, the roles claim lets it past the admin gate, survives a reload", async ({ page }) => {
  await page.goto("/poison");

  // ListPoison is an admin RPC: the roles claim is what lets alice past the gate.
  await useJWT(page, mintJWT("alice", { roles: ["admin"] }));
  await expect(subject(page)).toHaveText("jwt · alice");
  await expect(page.getByText("Admin role required")).toHaveCount(0);
  await expect(page.getByRole("alert")).toContainText("admin.no_database");

  // Survives a reload like the API key does.
  await page.reload();
  await expect(subject(page)).toHaveText("jwt · alice");
  await expect(page.getByText("Admin role required")).toHaveCount(0);
  await expect(page.getByRole("alert")).toContainText("admin.no_database");
});

test("mutations carry the credential set after the page loaded", async ({ page }) => {
  await page.goto("/prune");
  await expect(page.getByText("Admin role required")).toBeVisible(); // anonymous: the page, and its form, are not there
  await expect(page.getByRole("button", { name: "Dry run", exact: true })).toHaveCount(0);

  // An admin key set after the page loaded: the form appears and the Prune it sends carries the key.
  await useKey(page, ADMIN_KEY);
  const sent = page.waitForRequest((r) => r.url().endsWith("/admin.v1.AdminService/Prune"));
  await page.getByRole("button", { name: "Dry run", exact: true }).click();
  expect((await sent).headers()["x-api-key"], "the key the console was given after it loaded").toBe(ADMIN_KEY);
  // The server accepted it: the answer is past the role check (no database here), not auth.unauthenticated / permission_denied.
  await expect(page.getByRole("alert")).toContainText("admin.no_database");

  // Clear (the header's): the page is gone again, nothing is sent as the admin.
  await page.locator("header").getByRole("button", { name: "Clear" }).click();
  await expect(page.getByText("Admin role required")).toBeVisible();
  await expect(page.getByRole("button", { name: "Dry run", exact: true })).toHaveCount(0);
});
