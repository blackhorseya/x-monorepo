import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY } from "../../playwright.config";
import { uid } from "../uid";
import { settle, useJWT } from "./helpers";
import { mintJWT } from "./jwt";

// CreateApiKey shows the new key's secret once, in a dialog. A success that arrives after the console has
// switched to another credential belongs to the old one: the secret was minted for the old caller's
// request, and showing it to the new credential's screen would hand it to the wrong person (#370).

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

test("a success for the previous credential does not show its secret to the new one", async ({ page }) => {
  const accountId = await makeAccount(page);
  await openKeyForm(page, accountId);

  // alice's CreateApiKey reaches the server and commits; the answer is held back.
  let committed!: () => void;
  const hasCommitted = new Promise<void>((r) => (committed = r));
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  let secret = "";
  await page.route("**/identity.v1.IdentityService/CreateApiKey", async (route) => {
    const res = await route.fetch();
    secret = ((await res.json()) as { secret: string }).secret;
    committed();
    await released;
    await route.fulfill({ response: res });
  });
  await page.getByRole("button", { name: "Create key" }).click();
  await hasCommitted;
  expect(secret).not.toBe(""); // positive control: the key exists, the answer is what is late

  await useJWT(page, admin(`e2e-bob-${uid()}`));
  release();
  await expect.poll(() => page.evaluate(() => performance.getEntriesByType("resource").filter((e) => e.name.endsWith("/CreateApiKey")).length)).toBeGreaterThanOrEqual(1);
  await settle(page);
  await page.waitForTimeout(300); // the answer has been delivered; only the page's reaction is left to wait for

  // A snapshot, not a retrying expect: a later render would not repair a regression.
  expect(await page.getByTestId("api-key-secret").count()).toBe(0);
  expect(await page.getByText(secret).count()).toBe(0);
});

// The other side: an answer for the credential still in use shows the secret.
test("a success for the current credential shows the secret", async ({ page }) => {
  const accountId = await makeAccount(page);
  await openKeyForm(page, accountId);
  await page.getByRole("button", { name: "Create key" }).click();
  await expect(page.getByTestId("api-key-secret")).toBeVisible();
});
