import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";

// The admin pages are their own chunks (lazy routes). A caller who is not an admin never needs
// them: the gate must show without fetching, or waiting for, the page's chunk — also when the
// router asks for the chunk early (direct load, link intent, in-app navigation). An admin still
// gets the early fetch. Runs in the auth project: OPS_KEY has no role, ADMIN_KEY the admin role.

const seed = (page: Page, key: string) =>
  page.addInitScript((k) => localStorage.setItem("console.credential", JSON.stringify({ kind: "api_key", value: k })), key);
const chunk = (name: string) => new RegExp(`/assets/${name}-[^/]+\\.js`);
const nav = (page: Page, name: string) => page.locator("header nav").getByRole("link", { name, exact: true });
// Client-side navigation, as a link click would do, without a new document load.
const navigate = (page: Page, path: string) =>
  page.evaluate((p) => {
    history.pushState(null, "", p);
    window.dispatchEvent(new PopStateEvent("popstate"));
  }, path);

for (const name of ["poison", "prune", "expire"]) {
  test(`no admin role: direct /${name} shows the gate without fetching its chunk`, async ({ page }) => {
    await seed(page, OPS_KEY);
    const chunks: string[] = [];
    page.on("request", (r) => {
      if (chunk(name).test(r.url())) chunks.push(r.url());
    });
    await page.goto(`/${name}`);
    await expect(page.getByText("Admin role required")).toBeVisible();
    await page.waitForTimeout(500);
    expect(chunks).toEqual([]);
  });
}

test("no admin role: the gate shows even when the admin chunk cannot be loaded", async ({ page }) => {
  await seed(page, OPS_KEY);
  let calls = 0;
  await page.route("**/assets/poison-*.js", (route) => {
    calls++;
    return route.abort("failed");
  });
  await page.goto("/poison");
  await expect(page.getByText("Admin role required")).toBeVisible();
  expect(calls).toBe(0);
});

test("no admin role: a slow admin chunk does not delay the gate", async ({ page }) => {
  await seed(page, OPS_KEY);
  let release!: () => void;
  const hold = new Promise<void>((r) => (release = r));
  await page.route("**/assets/poison-*.js", async (route) => {
    await hold;
    await route.continue();
  });
  try {
    await page.goto("/poison");
    await expect(page.getByText("Admin role required")).toBeVisible({ timeout: 1_500 });
  } finally {
    release();
  }
});

test("no admin role, already known: navigating to an admin page fetches no chunk", async ({ page }) => {
  await seed(page, OPS_KEY);
  const probe = page.waitForResponse((r) => r.url().endsWith("/identity.v1.IdentityService/ListAccounts"));
  await page.goto("/stock");
  expect((await probe).status()).toBe(403);
  await expect(nav(page, "Stock")).toBeVisible();

  const chunks: string[] = [];
  page.on("request", (r) => {
    if (chunk("poison").test(r.url())) chunks.push(r.url());
  });
  await navigate(page, "/poison");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await page.waitForTimeout(500);
  expect(chunks).toEqual([]);
});

test("admin: link intent still preloads the admin chunk", async ({ page }) => {
  await seed(page, ADMIN_KEY);
  await page.goto("/stock");
  await expect(nav(page, "Poison")).toBeVisible();
  const chunks: string[] = [];
  page.on("request", (r) => {
    if (chunk("poison").test(r.url())) chunks.push(r.url());
  });
  await nav(page, "Poison").hover();
  await expect.poll(() => chunks.length).toBeGreaterThan(0);
});
