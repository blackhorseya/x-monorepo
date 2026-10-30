import { expect, test, type Page } from "@playwright/test";
import { mockIdp, signIn, stubConfig } from "./oidc";
import { subject } from "./helpers";
import { uid } from "../uid";

// /accounts/$id for an admin who signed in through OIDC (the stub IdP's token carries the admin
// role, the real rpc accepts it): invite someone, see what the contract says about sign-in
// bindings, sign the account out everywhere. There is no RPC that lists bindings or invites, so
// the page shows only what the contract returns: sessions_revoke_time and the invite just made.

const title = (page: Page, text: string) => page.locator('[data-slot="card-title"]', { hasText: text });

async function openNewAccount(page: Page) {
  await stubConfig(page);
  await mockIdp(page, { claims: { roles: ["admin"] } });
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await page.goto("/accounts");
  const name = `e2e-oidc-${uid()}`;
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByRole("button", { name: "Create account" }).click();
  await page.getByRole("row").filter({ hasText: name }).getByRole("link", { name }).click();
  await expect(page).toHaveURL(/\/accounts\/.+/);
  return name;
}

test("admin: create an invite for an email — shown with its 7-day expiry, nothing secret", async ({ page }) => {
  await openNewAccount(page);
  await expect(title(page, "Invite")).toBeVisible();
  await page.getByLabel("Invite email").fill("pat@example.com");
  await page.getByRole("button", { name: "Create invite" }).click();

  const made = page.getByTestId("invite-made");
  await expect(made).toContainText("pat@example.com");
  await expect(made).toContainText(/expires/i);
  await expect(made).toContainText(/single use|once/i);
  await expect(page.getByLabel("Invite email")).toHaveValue("");

  // A second one is listed too; the page says what it cannot show.
  await page.getByLabel("Invite email").fill("sam@example.com");
  await page.getByRole("button", { name: "Create invite" }).click();
  await expect(page.getByTestId("invite-made")).toHaveCount(2);
  await expect(title(page, "Sign-in bindings")).toBeVisible();
  await expect(page.getByText(/not shown in the console yet/i)).toBeVisible();
});

test("an invalid invite email is refused by the server and shown", async ({ page }) => {
  await openNewAccount(page);
  await page.getByLabel("Invite email").fill("a"); // below the contract's min length
  await page.getByRole("button", { name: "Create invite" }).click();
  await expect(page.getByRole("alert")).toContainText(/email/i);
  await expect(page.getByTestId("invite-made")).toHaveCount(0);
});

test("admin: revoke all sessions asks first, then shows when it happened", async ({ page }) => {
  await openNewAccount(page);
  await expect(title(page, "Sessions")).toBeVisible();
  await expect(page.getByText("Never revoked")).toBeVisible();

  await page.getByRole("button", { name: "Revoke sessions" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByText("Never revoked")).toBeVisible();

  await page.getByRole("button", { name: "Revoke sessions" }).click();
  await expect(page.getByRole("alertdialog")).toContainText(/refresh token/i); // "sign out of all devices", not a compromise response
  await page.getByRole("alertdialog").getByRole("button", { name: "Revoke all sessions" }).click();
  await expect(page.getByText(/Revoked at/)).toBeVisible();
  await expect(page.getByText("Never revoked")).toHaveCount(0);
});

test("a non-admin OIDC user gets no invite or revoke controls", async ({ page }) => {
  await stubConfig(page);
  await mockIdp(page);
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await page.goto("/accounts/some-account");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await expect(page.getByRole("button", { name: "Create invite" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Revoke sessions" })).toHaveCount(0);
});
