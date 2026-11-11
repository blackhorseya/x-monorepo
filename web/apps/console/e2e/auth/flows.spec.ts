import { expect, test } from "@playwright/test";
import { ADMIN_KEY } from "../../playwright.config";
import { subject, useJWT, useKey } from "./helpers";
import { mintJWT } from "./jwt";

// The parts of the credential story auth.spec.ts leaves out: the JWT kind
// (header shows the token's sub; expired / wrong issuer / wrong secret are
// auth.invalid_credential; the roles claim is what the admin gate reads), and
// that mutations carry the credential too — the interceptor is read at call
// time, so a key set after the page loaded must reach the next Prune.
// Past the role gate this server has no database, so an admin sees the server's own answer, admin.no_database.

test("JWT: sub in the header, rejected variants name the reason", async ({
  page,
}) => {
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

  for (const bad of [
    mintJWT("alice", { roles: ["admin"], exp: Math.floor(Date.now() / 1000) - 60 }), // expired
    mintJWT("alice", { roles: ["admin"], iss: "https://someone-else.example.com" }), // wrong issuer
    mintJWT("alice", { roles: ["admin"] }, "another-secret-0123456789abcdef0123456789abcdef"), // wrong key
  ]) {
    await useJWT(page, bad);
    await expect(page.getByText("Admin role required")).toBeVisible(); // refused: not an admin
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
