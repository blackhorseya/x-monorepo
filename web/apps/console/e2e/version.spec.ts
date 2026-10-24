import { expect, test } from "@playwright/test";

// task build sets no ldflags and task web-build no VITE_CONSOLE_VERSION, so both sides
// report "dev"; what is checked is that the footer reads the server's from GET /api/version
// and does not call a dev / dev pair a mismatch.
test("footer shows the console and server builds", async ({ page }) => {
  await page.goto("/orders");
  const v = page.getByTestId("server-version");
  await expect(v).toHaveText("console dev · server dev (none)");
  await expect(v).toHaveAttribute("title", /^server built unknown with go\d/);
});
