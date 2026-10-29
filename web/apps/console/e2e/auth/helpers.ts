import type { Page } from "@playwright/test";

// The header's credential control: pick the kind, paste the value, Use.
export async function useKey(page: Page, key: string) {
  await useCredential(page, "api_key", key);
}

export async function useJWT(page: Page, token: string) {
  await useCredential(page, "bearer", token);
}

async function useCredential(
  page: Page,
  kind: "api_key" | "bearer",
  value: string
) {
  await page.getByRole("button", { name: "Credential" }).click();
  await page.getByLabel("Kind").selectOption(kind);
  await page.getByLabel("Value").fill(value);
  await page.getByRole("button", { name: "Use" }).click();
}

// What the header says the console calls as (src/auth.ts subject()).
export const subject = (page: Page) =>
  page.locator("header").getByText(/^(anonymous|api key|jwt)/);

// Lets everything already queued in the page run: pending microtasks, two frames of rendering
// and one timer turn. Used after a response or navigation has provably happened, so that a
// negative assertion ("nothing was fetched / shown") is made after the page had its chance to
// react, not after a guessed delay.
export const settle = (page: Page) =>
  page.evaluate(
    () =>
      new Promise<void>((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(resolve, 0)))
      )
  );
