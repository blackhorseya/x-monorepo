import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY } from "../../playwright.config";
import { uid } from "../uid";
import { settle, useJWT, useKey } from "./helpers";
import { mintJWT } from "./jwt";

// The invariant these tests guard: the admin pages' state does not outlive a change of credential.
//
// CreateAccount (/accounts) and CreateInvite (/accounts/$id) clear their form (and CreateInvite lists
// the invite it made) in onSuccess. A mutation's onSuccess still runs after the credential was changed
// (useResetOnCredentialChange resets the mutation's state, it does not cancel the call). What keeps that
// late answer from touching the new credential's form is AdminGate (src/components/admin.tsx): every
// change of credential is a new generation (src/auth.ts, a counter that only goes up), the admin probe
// asks again for it, and while it has no answer AdminGate renders nothing, so the card is unmounted and
// comes back with fresh state. The late setState lands on the unmounted instance.
//
// So these pass on purpose, and they stop passing if AdminGate ever keeps its children mounted while the
// probe is pending, or if a card is moved out from under it. Then the callbacks need the generation check
// the order form has (routes/orders.tsx, #370). Fault injection: make AdminGate return `children` while
// `probe.isPending` and the three "previous credential" tests below fail (the form is cleared).
//
// The server is real (auth project); only the order of the answer is controlled: the request reaches the
// server and commits, the answer is held back until the credential has been switched.

type Held = { committed: Promise<void>; release: () => void; delivered: Promise<void> };

// Holds the answer of every call to `method` once, after the server has handled it.
async function holdAnswer(page: Page, method: string): Promise<Held> {
  let committed!: () => void;
  const hasCommitted = new Promise<void>((r) => (committed = r));
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  let delivered!: () => void;
  const wasDelivered = new Promise<void>((r) => (delivered = r));
  let first = true;
  await page.route(`**/identity.v1.IdentityService/${method}`, async (route) => {
    if (!first) return route.continue();
    first = false;
    const res = await route.fetch();
    committed();
    await released;
    await route.fulfill({ response: res });
    delivered();
  });
  return { committed: hasCommitted, release, delivered: wasDelivered };
}

const otherAdmin = () => mintJWT(`e2e-admin-b-${uid()}`, { roles: ["admin"] });

test("CreateAccount: a late answer for the previous credential leaves the new credential's form alone", async ({ page }) => {
  await page.goto("/accounts");
  await useKey(page, ADMIN_KEY);
  const held = await holdAnswer(page, "CreateAccount");
  await page.getByLabel("Name", { exact: true }).fill(`e2e-late-a-${uid()}`);
  await page.getByLabel("Roles").fill("viewer");
  await page.getByRole("button", { name: "Create account" }).click();
  await held.committed; // positive control: the server created A's account

  await useJWT(page, otherAdmin());
  await page.getByLabel("Name", { exact: true }).fill("typed-by-b");
  await page.getByLabel("Roles").fill("roles-of-b");

  held.release();
  await held.delivered; // positive control: the old answer has been handed to the page
  await settle(page);
  await page.waitForTimeout(300);

  // A snapshot, not a retrying expect: nothing later repairs a cleared field.
  expect(await page.getByLabel("Name", { exact: true }).inputValue()).toBe("typed-by-b");
  expect(await page.getByLabel("Roles").inputValue()).toBe("roles-of-b");
});

test("CreateAccount: an answer for the current credential clears the form", async ({ page }) => {
  await page.goto("/accounts");
  await useKey(page, ADMIN_KEY);
  const name = `e2e-late-ok-${uid()}`;
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByLabel("Roles").fill("viewer");
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.getByRole("row").filter({ hasText: name })).toHaveCount(1);
  await expect(page.getByLabel("Name", { exact: true })).toHaveValue("");
  await expect(page.getByLabel("Roles")).toHaveValue("");
});

async function newAccountPage(page: Page) {
  const name = `e2e-late-inv-${uid()}`;
  await page.goto("/accounts");
  await useKey(page, ADMIN_KEY);
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByRole("button", { name: "Create account" }).click();
  await page.getByRole("row").filter({ hasText: name }).getByRole("link", { name }).click();
  await expect(page).toHaveURL(/\/accounts\/.+/);
}

test("CreateInvite: a late answer for the previous credential neither lists its invite nor clears the new credential's field", async ({ page }) => {
  await newAccountPage(page);
  const held = await holdAnswer(page, "CreateInvite");
  await page.getByLabel("Invite email").fill("invited-by-a@example.com");
  await page.getByRole("button", { name: "Create invite" }).click();
  await held.committed; // positive control: the server created A's invite

  await useJWT(page, otherAdmin());
  await page.getByLabel("Invite email").fill("typed-by-b@example.com");

  held.release();
  await held.delivered;
  await settle(page);
  await page.waitForTimeout(300);

  expect(await page.getByLabel("Invite email").inputValue()).toBe("typed-by-b@example.com");
  expect(await page.getByTestId("invite-made").count()).toBe(0);
});

test("CreateInvite: an answer for the current credential lists the invite and clears the field", async ({ page }) => {
  await newAccountPage(page);
  await page.getByLabel("Invite email").fill("invited@example.com");
  await page.getByRole("button", { name: "Create invite" }).click();
  await expect(page.getByTestId("invite-made")).toHaveCount(1);
  await expect(page.getByLabel("Invite email")).toHaveValue("");
});

// Going back to the first credential is not going back to its generation: every change of credential
// is a new generation, so there is no cached probe answer to skip the pending step, and the page is
// unmounted again. The old answer must not touch the form of the third generation either.
test("CreateAccount: back on the first credential (a new generation), the first answer still leaves the form alone", async ({ page }) => {
  await page.goto("/accounts");
  await useKey(page, ADMIN_KEY);
  const held = await holdAnswer(page, "CreateAccount");
  await page.getByLabel("Name", { exact: true }).fill(`e2e-late-aba-${uid()}`);
  await page.getByRole("button", { name: "Create account" }).click();
  await held.committed;

  await useJWT(page, otherAdmin());
  await expect(page.getByLabel("Name", { exact: true })).toBeVisible();
  await useKey(page, ADMIN_KEY); // the first credential again
  await page.getByLabel("Name", { exact: true }).fill("typed-after-return");

  held.release();
  await held.delivered;
  await settle(page);
  await page.waitForTimeout(300);
  expect(await page.getByLabel("Name", { exact: true }).inputValue()).toBe("typed-after-return");
});
