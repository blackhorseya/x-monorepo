import { expect, test, type Page } from "@playwright/test";

// The stock and reservations lists poll their first page only (like the orders list,
// polling.spec.ts), not every page they have loaded. The list responses are scripted (three pages,
// 45 rows) so this holds in both e2e modes and does not depend on a worker. A fake clock (page.clock)
// stands in for the 2s tick; every window starts with a positive control (the first page *was*
// polled), so "no later-page requests" cannot pass because nothing polled at all.

const PAGES = [
  { token: "", size: 20, next: "p2" },
  { token: "p2", size: 20, next: "p3" },
  { token: "p3", size: 5, next: "" },
];

type Kind = {
  name: string;
  path: string;
  url: string;
  // The rows of page `p` as the server would send them; row 0 of the first page carries `mark`.
  body: (p: (typeof PAGES)[number], page: number, mark: "A" | "B") => object;
  // The text that shows `mark` on the first row of the first page.
  shows: (page: Page, mark: "A" | "B") => ReturnType<Page["locator"]>;
};

const kinds: Kind[] = [
  {
    name: "stock",
    path: "inventory.v1.InventoryService/ListStocks",
    url: "/stock",
    body: (p, page, mark) => ({
      stocks: Array.from({ length: p.size }, (_, i) => ({
        sku: `LP-${page}-${String(i).padStart(2, "0")}`,
        onHand: page === 0 && i === 0 ? (mark === "A" ? 1111 : 2222) : 5,
        reserved: 0,
        available: 5,
      })),
      nextPageToken: p.next,
    }),
    shows: (page, mark) => page.getByRole("row").filter({ hasText: "LP-0-00" }).getByRole("cell", { name: mark === "A" ? "1111" : "2222" }),
  },
  {
    name: "reservations",
    path: "inventory.v1.InventoryService/ListReservations",
    url: "/reservations",
    body: (p, page, mark) => ({
      reservations: Array.from({ length: p.size }, (_, i) => ({
        orderId: `lp-${page}-${String(i).padStart(2, "0")}`,
        status: page === 0 && i === 0 && mark === "B" ? "RESERVATION_STATUS_RELEASED" : "RESERVATION_STATUS_RESERVED",
        lines: [],
        shortfallSkus: [],
      })),
      nextPageToken: p.next,
    }),
    shows: (page, mark) =>
      page.getByRole("row").filter({ hasText: "lp-0-00" }).getByTestId("reservation-status").filter({ hasText: mark === "A" ? "RESERVED" : "RELEASED" }),
  },
];

// Answers the list from `state.mark`; counts first-page and later-page requests. `hold` (when set)
// is called with the first-page response about to be sent, and decides when it is delivered.
function script(page: Page, kind: Kind, state: { mark: "A" | "B"; hold?: (deliver: () => Promise<void>) => Promise<void> }) {
  const calls = { first: 0, later: 0 };
  return page
    .route(`**/${kind.path}`, async (route) => {
      const { pageToken = "" } = route.request().postDataJSON() as { pageToken?: string };
      const index = PAGES.findIndex((p) => p.token === pageToken);
      if (index === 0) calls.first++;
      else calls.later++;
      const json = kind.body(PAGES[index], index, state.mark);
      const hold = index === 0 ? state.hold : undefined;
      state.hold = undefined; // only the first poll that gets here is delayed
      if (hold) await hold(() => route.fulfill({ json }));
      else await route.fulfill({ json });
    })
    .then(() => calls);
}

for (const kind of kinds) {
  test.describe(kind.name, () => {
    test("with three pages loaded, a poll tick asks for the first page only", async ({ page }) => {
      await page.clock.install();
      const calls = await script(page, kind, { mark: "A" });
      await page.goto(kind.url);
      await expect(kind.shows(page, "A")).toBeVisible();
      for (const n of [40, 45]) {
        await page.getByRole("button", { name: "Load more", exact: true }).click();
        await expect(page.getByRole("row")).toHaveCount(n + 1);
      }
      expect(calls.later).toBeGreaterThanOrEqual(2); // the pages were loaded

      const first = calls.first;
      const later = calls.later;
      for (let i = 0; i < 3; i++) {
        await page.clock.fastForward(2_100);
        await page.waitForTimeout(400);
      }
      expect(calls.first).toBeGreaterThan(first); // positive control: the first page is polled
      expect(calls.later).toBe(later); // ... and only the first page
    });

    // The poll lives outside the query, so a refetch (here: the tab coming back) cannot cancel it. A
    // poll answered after that refetch holds older rows and must not overwrite the newer ones.
    test("a poll answered after a refetch does not undo it", async ({ page }) => {
      const state: Parameters<typeof script>[2] = { mark: "A" };
      await script(page, kind, state);
      await page.goto(kind.url);
      await expect(kind.shows(page, "A")).toBeVisible();

      let held!: () => void;
      const captured = new Promise<void>((r) => (held = r));
      let release!: () => void;
      const released = new Promise<void>((r) => (release = r));
      state.hold = async (deliver) => {
        held(); // the server has answered (still A) ...
        await released; // ... but the browser gets it late
        await deliver();
      };
      await captured;
      state.mark = "B"; // the world moved on; what the refetch below reads
      await page.waitForTimeout(1_100); // past the list's 1s staleTime, or focus would not refetch
      await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
      await expect(kind.shows(page, "B")).toBeVisible();

      release();
      await page.waitForTimeout(300); // far below the 2s tick: only the stale response can act now
      // A snapshot, not a retrying expect: the next tick would repair a regression and hide it.
      expect(await kind.shows(page, "B").count()).toBe(1);
      expect(await kind.shows(page, "A").count()).toBe(0);
    });
  });
}
