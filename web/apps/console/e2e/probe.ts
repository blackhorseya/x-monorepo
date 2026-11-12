import type { Page, Request, Route } from "@playwright/test";

// The console's admin probe (src/queries.ts adminProbeQuery) is admin.v1.AdminService/ListPoison with limit 1.
// The Poison page's own list asks the same RPC with an empty request ({}), polled every 5 s, so a stub meant
// for one of the two has to tell them apart by the request body. These are the two bodies, in one place.
export const LIST_POISON = "admin.v1.AdminService/ListPoison";
/** In the body of the probe (and never in the page's own list). */
export const PROBE_BODY = '"limit":1';
/** The page's own list: an empty request. `{}` is not a substring of the probe's body. */
export const PAGE_BODY = "{}";

export const isProbe = (r: Request) => r.url().endsWith(`/${LIST_POISON}`) && (r.postData() ?? "").includes(PROBE_BODY);

/**
 * Handle the probe's requests; every other request goes on to the next handler (route.fallback).
 * Register it before page.goto.
 */
export function routeProbe(page: Page, handler: (route: Route) => Promise<void> | void) {
  return page.route(`**/${LIST_POISON}`, (route) => (isProbe(route.request()) ? handler(route) : route.fallback()));
}
