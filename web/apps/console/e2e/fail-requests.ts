import type { Page } from "@playwright/test";

// Fault injection for the console's RPCs, aimed at one RPC method (and optionally one caller): for
// specs that need a server failure or a slow answer at a precise moment. Every other request goes
// on untouched, and the handlers hand it to the next one (route.fallback), so several of these on
// one page, and a spec's own page.route, compose. Counters are per call.
//
// method  the RPC's name, the last segment of the URL ("ListOrders", "QuotePrices"); matched exactly.
// key     which callers: omitted = any; a string = calls sent with that X-API-Key; null = calls sent
//         with no X-API-Key (the anonymous ones: the console's first requests before a credential is set).
export type Target = { method: string; key?: string | null };

function matches(page: Page, { method, key }: Target) {
  return (url: string, headers: Record<string, string>) => {
    const path = new URL(url).pathname;
    if (path.slice(path.lastIndexOf("/") + 1) !== method) return false;
    if (key === undefined) return true;
    const sent = headers["x-api-key"];
    return key === null ? sent === undefined : sent === key;
  };
}

// ---- failRequests: the next `times` matching calls fail -------------------------------------
//
//   const fault = await failRequests(page, { method: "ListOrders", key: OPS_KEY });   // before page.goto
//   ...
//   await expect.poll(() => fault.failed()).toBe(1);   // the fault was really hit: no vacuous run
//
// times   how many matching calls fail (default 1); the ones after that pass through.
// mode    "unavailable" (default) answers 503 with a Connect `unavailable` error; "reset" drops the
//         connection (the browser sees "Failed to fetch").
export type FailRequestsOptions = Target & { times?: number; mode?: "unavailable" | "reset" };

export type RequestFault = {
  /** Calls that matched (method and key), failed or not. */
  matched: () => number;
  /** Calls that were failed. */
  failed: () => number;
};

export async function failRequests(page: Page, options: FailRequestsOptions): Promise<RequestFault> {
  const { times = 1, mode = "unavailable" } = options;
  const hit = matches(page, options);
  let matched = 0;
  let failed = 0;
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    if (!hit(request.url(), request.headers())) return route.fallback();
    matched++;
    if (failed >= times) return route.fallback();
    failed++;
    if (mode === "reset") return route.abort("connectionreset");
    return route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ code: "unavailable", message: "injected by failRequests" }),
    });
  });
  return { matched: () => matched, failed: () => failed };
}

// ---- holdRequests: matching calls are parked until release() ---------------------------------
//
//   const held = await holdRequests(page, { method: "ListOrders", key: null });   // before page.goto
//   await page.goto("/orders");
//   await held.arrived();          // the anonymous ListOrders is on the wire, unanswered
//   await useKey(page, OPS_KEY);   // the credential changes while the first answer is still out
//   held.release();                // now the stale answer may land
//
// A parked call is not sent to the server until release(); after release() matching calls pass straight
// through and are not counted: arrived() and count() only ever speak of calls that really were parked, so
// they are safe as a positive control ("the hold was hit") even when release() runs in a cleanup path.
// Several calls can be parked at once.
export type RequestHold = {
  /** Resolves once the first matching call is parked; never resolves if none was before release(). */
  arrived: () => Promise<void>;
  /** Calls parked so far. */
  count: () => number;
  /** Let every parked call, and every later one, go to the server. */
  release: () => void;
};

export async function holdRequests(page: Page, target: Target): Promise<RequestHold> {
  const hit = matches(page, target);
  let count = 0;
  let released = false;
  let open!: () => void;
  const gate = new Promise<void>((r) => (open = r));
  const release = () => {
    released = true;
    open();
  };
  let arrive!: () => void;
  const arrival = new Promise<void>((r) => (arrive = r));
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    if (!hit(request.url(), request.headers()) || released) return route.fallback();
    count++;
    arrive();
    await gate;
    return route.fallback();
  });
  return { arrived: () => arrival, count: () => count, release };
}
