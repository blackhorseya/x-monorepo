import { expect, test } from "@playwright/test";

// task build sets no ldflags, so the smoke server reports buildinfo's defaults;
// what is checked is that the footer reads them from GET /api/version.
test("footer shows the server build", async ({ page }) => {
  await page.goto("/orders");
  const v = page.getByTestId("server-version");
  await expect(v).toHaveText("server dev (none)");
  await expect(v).toHaveAttribute("title", /^built unknown with go\d/);
});
