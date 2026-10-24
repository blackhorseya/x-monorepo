import { expect, test } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { subject, useJWT, useKey } from "./helpers";
import { mintJWT } from "./jwt";

// The parts of the credential story auth.spec.ts leaves out: the JWT kind
// (header shows the token's sub; expired / wrong issuer / wrong secret are
// auth.invalid_credential; the roles claim is what the admin gate reads), and
// that mutations carry the credential too — the interceptor is read at call
// time, so a key set after the page loaded must reach the next Create / Set.

const customer = (t: string) => `e2e-auth-${t}-${Date.now()}`;

test("JWT: sub in the header, rejected variants name the reason", async ({
  page,
}) => {
  await page.goto("/orders");

  await useJWT(page, mintJWT("alice"));
  await expect(subject(page)).toHaveText("jwt · alice");
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(
    page
      .getByRole("row")
      .filter({ hasText: /No orders|PENDING|CANCELLED/ })
      .first()
  ).toBeVisible();

  // Survives a reload like the API key does.
  await page.reload();
  await expect(subject(page)).toHaveText("jwt · alice");
  await expect(page.getByRole("alert")).toHaveCount(0);

  for (const bad of [
    mintJWT("alice", { exp: Math.floor(Date.now() / 1000) - 60 }), // expired
    mintJWT("alice", { iss: "https://someone-else.example.com" }), // wrong issuer
    mintJWT("alice", {}, "another-secret-0123456789abcdef0123456789abcdef"), // wrong key
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
  await page.getByRole("button", { name: "Dry run", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("auth.permission_denied");

  await useJWT(page, mintJWT("carol", { roles: ["admin"] }));
  await page.getByRole("button", { name: "Dry run", exact: true }).click();
  // Past the gate: this server has no database, so the next error is admin's own.
  await expect(page.getByRole("alert")).toContainText("admin.no_database");
});

test("mutations carry the credential set after the page loaded", async ({
  page,
}) => {
  const id = customer("write");
  await page.goto("/orders");
  await expect(page.getByRole("alert").first()).toContainText(
    "auth.unauthenticated"
  );

  // Anonymous Create is refused by the server, and says so next to the form.
  await page.getByLabel("Customer").first().fill(id);
  await page.getByRole("button", { name: "Create order" }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "auth.unauthenticated" })
  ).toHaveCount(2);

  // A role-less key is enough for OrderService: the list recovers at once, the
  // form's own alert stays until its next attempt — which now succeeds.
  await useKey(page, OPS_KEY);
  await expect(page.getByRole("alert")).toHaveCount(1);
  await page.getByLabel("Customer").first().fill(id);
  await page.getByRole("button", { name: "Create order" }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByLabel("Customer").nth(1).fill(id);
  await page.getByRole("button", { name: "Filter" }).click();
  const row = page.getByRole("row").filter({ hasText: id });
  await expect(row).toHaveCount(1);
  await expect(row.getByText("PENDING")).toBeVisible();

  // Cancel too, and with the other kind of credential.
  await useJWT(page, mintJWT("dave"));
  await row.getByRole("button", { name: "Cancel" }).click();
  await expect(row.getByText("CANCELLED")).toBeVisible();

  // Clear (the header's; the filter form has its own): the next write is anonymous again.
  await page.locator("header").getByRole("button", { name: "Clear" }).click();
  await page.getByLabel("Customer").first().fill(customer("anon"));
  await page.getByRole("button", { name: "Create order" }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "auth.unauthenticated" }).first()
  ).toBeVisible();
});

test("SetStock under auth: refused anonymous, ok with a key", async ({
  page,
}) => {
  const sku = `E2E-AUTH-${Date.now()}`;
  await page.goto("/stock");
  await page.getByLabel("SKU", { exact: true }).fill(sku);
  await page.getByLabel("On hand").fill("3");
  await page.getByRole("button", { name: "Set", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "auth.unauthenticated" }).first()
  ).toBeVisible();

  await useKey(page, ADMIN_KEY);
  await page.getByLabel("SKU", { exact: true }).fill(sku);
  await page.getByLabel("On hand").fill("3");
  await page.getByRole("button", { name: "Set", exact: true }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
  const row = page.getByRole("row").filter({ hasText: sku });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText("3");
});
