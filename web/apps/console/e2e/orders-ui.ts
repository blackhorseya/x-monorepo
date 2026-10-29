import { expect, type Locator, type Page } from "@playwright/test";

// Pieces of the /orders page the specs share. The page has two "Customer" fields (create form,
// list filter), so they are told apart by their card, not by their position.
const card = (page: Page, title: RegExp) =>
  page.locator('[data-slot="card"]', { has: page.locator('[data-slot="card-title"]', { hasText: title }) });
export const createCard = (page: Page) => card(page, /^Create order$/);
export const filterCard = (page: Page) => card(page, /^Orders$/);

// Cancel and Complete ask first: answer the open dialog with its action button.
export async function confirm(page: Page, action: "Cancel order" | "Complete order") {
  await page.getByRole("alertdialog").getByRole("button", { name: action, exact: true }).click();
  await expect(page.getByRole("alertdialog")).toHaveCount(0);
}

// The cell of `row` under the column headed `header` (found in the table, not counted by hand).
export async function cellUnder(page: Page, row: Locator, header: string): Promise<Locator> {
  const index = await page.getByRole("columnheader", { name: header, exact: true }).first().evaluate((th) => (th as HTMLTableCellElement).cellIndex);
  return row.getByRole("cell").nth(index);
}
