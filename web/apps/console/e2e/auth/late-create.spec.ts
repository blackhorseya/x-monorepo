import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY } from "../../playwright.config";
import { uid } from "../uid";
import { useJWT } from "./helpers";
import { mintJWT } from "./jwt";

// CreateApiKey shows the new key's secret once, in a dialog. The dialog is modal, so the credential control cannot
// be reached while it is open: a success arriving after a credential switch (#370) has no way to be staged from the UI
// any more, and only the answer for the credential in use is checked here.

const admin = (sub: string) => mintJWT(sub, { roles: ["admin"] });

async function makeAccount(page: Page) {
  const res = await page.request.post("/api/identity.v1.IdentityService/CreateAccount", {
    headers: { "X-API-Key": ADMIN_KEY },
    data: { name: `e2e-late-${uid()}` },
  });
  expect(res.ok(), `CreateAccount: ${res.status()} ${await res.text()}`).toBe(true);
  return ((await res.json()) as { account: { id: string } }).account.id;
}

async function openKeyForm(page: Page, accountId: string) {
  await page.goto(`/accounts/${accountId}`);
  await useJWT(page, admin(`e2e-alice-${uid()}`));
  await page.getByRole("button", { name: "New API key" }).click();
  await page.getByLabel("Key name").fill(`k-${uid()}`);
}

test("a success for the current credential shows the secret", async ({ page }) => {
  const accountId = await makeAccount(page);
  await openKeyForm(page, accountId);
  await page.getByRole("button", { name: "Create key" }).click();
  await expect(page.getByTestId("api-key-secret")).toBeVisible();
});
