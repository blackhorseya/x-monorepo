import { expect, test, type APIRequestContext } from "@playwright/test";
import { failRequests } from "./fail-requests";
import { uid } from "./uid";

// A read that fails ONCE for a reason that passes (the server restarting, a connection reset) must not
// leave the console showing an error forever; a read that fails because the answer is "no" must keep
// showing it, and must not be asked again. The rpc is the real bin/rpc: failRequests (e2e/fail-requests.ts)
// only breaks the calls a test names, everything else goes on to it. Real time, no fake clock: the backoff
// is the thing under test. Deadlines are generous against the 3.5s retry budget (src/transient.ts).
const RECOVERY_MS = 15_000;

// One account of the test's own, so the list a test reads has a row it can name.
async function makeAccount(request: APIRequestContext) {
  const name = `e2e-tr-${uid()}`;
  const res = await request.post("/api/identity.v1.IdentityService/CreateAccount", { data: { name } });
  expect(res.ok(), `CreateAccount: ${res.status()} ${await res.text()}`).toBe(true);
  return name;
}

test("(a) the first ListAccounts fails once (503): the Accounts list recovers by itself", async ({ page, request }) => {
  const name = await makeAccount(request);
  const fault = await failRequests(page, { method: "ListAccounts", times: 1 }); // the first call: 503

  await page.goto("/accounts");
  await expect(page.getByRole("row").filter({ hasText: name })).toBeVisible({ timeout: RECOVERY_MS });
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(fault.failed(), "the refusal was really served").toBe(1);
  expect(fault.matched(), "and the list was asked again").toBeGreaterThanOrEqual(2);
});

test("(a2) the first ListAccounts resets the connection: the Accounts list recovers by itself", async ({ page, request }) => {
  const name = await makeAccount(request);
  const fault = await failRequests(page, { method: "ListAccounts", mode: "reset" });

  await page.goto("/accounts");
  await expect(page.getByRole("row").filter({ hasText: name })).toBeVisible({ timeout: RECOVERY_MS });
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(fault.failed()).toBe(1);
  expect(fault.matched()).toBeGreaterThanOrEqual(2);
});

// (c) and (d) are guards, green before the fix too: the console never retried an answer or a mutation, and the
// fix must not start to.
test("(c) PermissionDenied is an answer: not retried, the alert stays, ListAccounts is asked once", async ({ page }) => {
  test.setTimeout(60_000);
  const list = await failRequests(page, { method: "ListAccounts", mode: "permission_denied", times: Infinity });

  await page.goto("/accounts");
  await expect(page.getByRole("alert")).toContainText("PermissionDenied", { timeout: RECOVERY_MS });
  // Past the whole retry budget (3.5s): nothing asks again.
  await page.waitForTimeout(8_000);
  await expect(page.getByRole("alert")).toContainText("PermissionDenied");
  expect(list.matched(), "ListAccounts requests").toBe(1);
});

test("(d) a CreateAccount that fails is not sent again by the console", async ({ page }) => {
  const create = await failRequests(page, { method: "CreateAccount", times: Infinity });

  await page.goto("/accounts");
  await page.getByLabel("Name").fill(`e2e-tr-${uid()}`);
  await page.getByRole("button", { name: "Create account" }).click();

  await expect(page.getByRole("alert")).toContainText(/unavailable/i, { timeout: RECOVERY_MS });
  await page.waitForTimeout(6_000); // past the retry budget: a retrying mutation would have sent again by now
  expect(create.matched(), "CreateAccount requests").toBe(1);
});

// ---- the admin probe (#483 review) -----------------------------------------------------------------------------------
// adminProbeQuery had `retry: 3` + its own delay (an answer such as InvalidArgument was asked four times). It now
// follows the one policy in src/transient.ts.

test("(e) the admin probe answered InvalidArgument is an answer: asked once, never retried", async ({ page }) => {
  const probe = await failRequests(page, { method: "ListAccounts", mode: "invalid_argument", times: Infinity });
  await page.goto("/poison");
  await expect.poll(() => probe.matched(), { timeout: RECOVERY_MS }).toBe(1);
  await page.waitForTimeout(6_000); // past the retry budget (3.5s): a retrying probe would have asked again by now
  expect(probe.matched(), "ListAccounts requests").toBe(1);
});

test("(f) the admin probe refused once with 503 is retried: the admin UI appears without a reload", async ({ page }) => {
  const probe = await failRequests(page, { method: "ListAccounts" }); // the first call: 503
  await page.goto("/poison");
  await expect(page.getByRole("link", { name: "Prune" })).toBeVisible({ timeout: RECOVERY_MS });
  expect([probe.failed(), probe.matched() >= 2], "refused once, then asked again").toEqual([1, true]);
});
