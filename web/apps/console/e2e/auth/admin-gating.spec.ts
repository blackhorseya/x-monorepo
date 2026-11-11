import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { isProbe, probeResponse, routeProbe } from "../probe";
import { useKey } from "./helpers";

// Which parts of the console a caller without the admin role sees. The server stays the gate
// (every admin RPC checks the role); this is what the console shows and, for the admin pages,
// what it does not even ask. Runs in the auth project: auth.mode=required, ADMIN_KEY holds the
// admin role, OPS_KEY none.

const adminPages = ["Poison", "Prune"] as const;
const navLink = (page: Page, name: string) => page.locator("header nav").getByRole("link", { name, exact: true });

test("no admin role: the nav leaves out every admin page", async ({ page }) => {
  await page.goto("/poison");
  // "no entry" only means something once the admin probe's answer for the role-less key (403) is in.
  const probed = probeResponse(page, (r) => r.key === OPS_KEY);
  await useKey(page, OPS_KEY);
  await probed;
  for (const name of adminPages) await expect(navLink(page, name)).toHaveCount(0);

  await useKey(page, ADMIN_KEY);
  for (const name of adminPages) await expect(navLink(page, name)).toBeVisible();
});

for (const path of ["/poison", "/prune"]) {
  test(`no admin role: ${path} shows the gate and sends no admin RPC but the probe`, async ({ page }) => {
    const adminCalls: string[] = [];
    page.on("request", (req) => {
      // The probe is the one admin RPC a non-admin page sends: it is how the console finds out the caller is not an admin.
      if (req.method() === "POST" && /\/admin\.v1\.AdminService/.test(req.url()) && !isProbe(req)) {
        adminCalls.push(req.url());
      }
    });
    await page.goto("/prune");
    const probed = probeResponse(page, (r) => r.key === OPS_KEY);
    await useKey(page, OPS_KEY);
    await probed;

    await page.goto(path);
    await expect(page.getByText("Admin role required")).toBeVisible();
    await page.waitForTimeout(500); // a page that mounted behind the gate would have asked by now
    expect(adminCalls).toEqual([]);

    await useKey(page, ADMIN_KEY);
    await expect(page.getByText("Admin role required")).toHaveCount(0);
  });
}

test("no admin role: Prune has no Dry run, an admin does", async ({ page }) => {
  await page.goto("/prune");
  await useKey(page, OPS_KEY);
  await expect(page.getByText("Admin role required")).toBeVisible();
  await expect(page.getByRole("button", { name: "Dry run", exact: true })).toHaveCount(0);

  await useKey(page, ADMIN_KEY);
  await expect(page.getByRole("button", { name: "Dry run", exact: true })).toBeVisible();
});

// "Not an admin" is an answer (PERMISSION_DENIED), not a failure: it is not retried.
test("no admin role: the probe is not retried", async ({ page }) => {
  let probes = 0;
  await routeProbe(page, (route) => {
    if (route.request().headers()["x-api-key"] === OPS_KEY) probes++;
    return route.continue();
  });
  await page.goto("/prune");
  await useKey(page, OPS_KEY);
  await expect.poll(() => probes).toBeGreaterThan(0);
  await page.waitForTimeout(500);
  const settled = probes;
  await page.waitForTimeout(3_000); // longer than any retry delay would be
  expect(probes).toBe(settled);
  await expect(page.getByText("Could not confirm admin access")).toHaveCount(0);
});
