import { expect, test, type Page } from "@playwright/test";
import { mockIdp, signIn, stubConfig } from "./oidc";
import { subject } from "./helpers";
import { uid } from "../uid";

// /accounts/$id "Sign-in bindings": the external identities bound to the account and its invites
// (IdentityService.ListAccountIdentities, admin only). Shown as the server returns them and nothing
// more: no token or secret is ever rendered, and a non-admin never even asks.

const LIST = "**/api/identity.v1.IdentityService/ListAccountIdentities";
const title = (page: Page, text: string) => page.locator('[data-slot="card-title"]', { hasText: text });

async function signedIn(page: Page, roles: string[]) {
  await stubConfig(page);
  await mockIdp(page, { claims: { roles } });
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
}

async function newAccount(page: Page) {
  await page.goto("/accounts");
  const name = `e2e-bind-${uid()}`;
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByRole("button", { name: "Create account" }).click();
  await page.getByRole("row").filter({ hasText: name }).getByRole("link", { name }).click();
  await expect(page).toHaveURL(/\/accounts\/.+/);
}

test("admin: identities (issuer, subject, when bound) and invites (email, status, expiry) are listed as the server returns them; nothing secret is shown", async ({ page }) => {
  await signedIn(page, ["admin"]);
  await newAccount(page);
  const accountId = page.url().split("/").pop()!;
  await page.route(LIST, (route) =>
    route.fulfill({
      status: 200,
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        identities: [
          { issuer: "https://idp.example.com/dex", subject: "sub-abc-123", accountId, bindTime: "2026-10-01T08:00:00Z", token: "SECRET-IDENTITY-TOKEN" },
        ],
        invites: [
          { id: "i-1", email: "pending@example.com", accountId, createTime: "2026-10-29T00:00:00Z", expireTime: "2026-11-05T00:00:00Z", status: "INVITE_STATUS_PENDING", token: "SECRET-INVITE-TOKEN", secret: "SECRET-2" },
          { id: "i-2", email: "claimed@example.com", accountId, createTime: "2026-10-20T00:00:00Z", expireTime: "2026-10-27T00:00:00Z", claimTime: "2026-10-21T00:00:00Z", status: "INVITE_STATUS_CLAIMED" },
          { id: "i-3", email: "old@example.com", accountId, createTime: "2026-10-01T00:00:00Z", expireTime: "2026-10-08T00:00:00Z", status: "INVITE_STATUS_EXPIRED" },
        ],
      }),
    }),
  );
  await page.reload();

  await expect(title(page, "Sign-in bindings")).toBeVisible();
  const identity = page.getByTestId("identity-row");
  await expect(identity).toHaveCount(1);
  await expect(identity).toContainText("https://idp.example.com/dex");
  await expect(identity).toContainText("sub-abc-123");
  await expect(identity).toContainText(/2026/);

  const invites = page.getByTestId("invite-row");
  await expect(invites).toHaveCount(3);
  await expect(invites.filter({ hasText: "pending@example.com" })).toContainText("PENDING");
  await expect(invites.filter({ hasText: "claimed@example.com" })).toContainText("CLAIMED");
  await expect(invites.filter({ hasText: "old@example.com" })).toContainText("EXPIRED");
  for (const row of [identity, invites]) await expect(row).not.toContainText(/SECRET/);
  await expect(page.locator("body")).not.toContainText("SECRET");
});

test("admin: an account with nothing bound says so, for identities and invites separately", async ({ page }) => {
  await signedIn(page, ["admin"]);
  await newAccount(page);
  await expect(page.getByText("No sign-ins bound yet")).toBeVisible();
  await expect(page.getByText("No invites yet")).toBeVisible();
  await expect(page.getByTestId("identity-row")).toHaveCount(0);
});

test("a failing list is shown as an error, not as an empty account", async ({ page }) => {
  await signedIn(page, ["admin"]);
  await newAccount(page);
  await page.route(LIST, (route) =>
    route.fulfill({ status: 500, headers: { "content-type": "application/json" }, body: JSON.stringify({ code: "internal", message: "boom" }) }),
  );
  await page.reload();
  await expect(page.getByRole("alert")).toContainText(/boom|internal/i);
  await expect(page.getByText("No sign-ins bound yet")).toHaveCount(0);
});

test("non-admin: no bindings card and the list RPC is never called", async ({ page }) => {
  await signedIn(page, []);
  const calls: string[] = [];
  page.on("request", (r) => r.url().includes("ListAccountIdentities") && calls.push(r.url()));
  await page.goto("/accounts/some-account");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await expect(title(page, "Sign-in bindings")).toHaveCount(0);
  await page.waitForTimeout(500);
  expect(calls).toEqual([]);
});
